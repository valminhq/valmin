import { session } from '$lib/state/session.svelte';
import { api } from './client';
import type { User } from './types';

/**
 * A standing instruction to enqueue a job on a cron expression (`12 §11`). The panel owns the
 * clock: a schedule enqueues a job row and the engine runs it, so a schedule that fires while
 * the instance is busy is recorded as a skipped run rather than queued behind the lock
 * (ADR-030).
 */
export interface Schedule {
	id: string;
	/** Null for a global schedule — one that sweeps every instance rather than naming one. */
	instance_id: string | null;
	kind: string;
	cron: string;
	enabled: boolean;
	last_run_at: string | null;
	next_run_at: string | null;
	/** Who set it up. Audit only: a schedule keeps firing after its author's grant is
	 * revoked, because backups that stop silently are the failure this milestone exists to
	 * prevent. */
	created_by: string | null;
	created_by_username: string | null;
	/** The location the expression is evaluated in. */
	timezone: string;
	/** Hold a due run while players are connected. Only restart and backup schedules set it. */
	wait_for_empty: boolean;
	/** How long a due run may be held before it runs with players connected. */
	max_deferral_seconds: number;
	/** How a server whose player count is unknown is treated: as occupied, or as empty. */
	unknown_players: 'wait' | 'run';
	/** When the clock began holding a due run, or null when no run is being held. */
	deferred_since: string | null;
	/** The latest the held run starts, or null when no run is being held. */
	deferred_until: string | null;
	/** The next few run times, earliest first; empty while the schedule is paused. */
	upcoming_runs: string[];
}

export interface CreateSchedule {
	instance_id?: string | null;
	kind: string;
	cron: string;
	timezone?: string;
	enabled?: boolean;
	wait_for_empty?: boolean;
	max_deferral_seconds?: number;
	unknown_players?: 'wait' | 'run';
}

export interface PatchSchedule {
	cron?: string;
	timezone?: string;
	enabled?: boolean;
}

interface Page<T> {
	items: T[];
	next_cursor: string | null;
	timezone: string;
}

/**
 * The kinds this screen offers for one instance, each with the capability that gates it. The
 * daemon holds the authoritative set and refuses anything else (`12 §3.1`); this is the label
 * and the gate, which no endpoint sends. The global kinds — `update_check` and `prune` — are
 * not here: they belong to no instance and need `schedules.global`. `playerAware` marks the
 * kinds that stop a running server and so can wait for its players to leave.
 */
export const scheduleKinds = [
	{ kind: 'backup', action: 'backups.create', label: 'Back up this server', playerAware: true },
	{ kind: 'restart', action: 'instance.restart', label: 'Restart this server', playerAware: true },
	{ kind: 'game_update', action: 'instance.update', label: 'Update the game', playerAware: false }
] as const;

/** The longest waits the editor offers, in seconds. The daemon accepts 60 to 86400. */
export const deferralChoices = [900, 1800, 3600, 7200, 14400, 28800, 86400];

/** A deferral in words: "15 minutes", "2 hours". */
export function deferral(seconds: number): string {
	const [n, unit] =
		seconds % 3600 === 0 ? [seconds / 3600, 'hour'] : [Math.round(seconds / 60), 'minute'];
	return `${n} ${unit}${n === 1 ? '' : 's'}`;
}

/** The label for a scheduled kind: the editor's own, or the kind with its underscores as spaces. */
export function kindLabel(kind: string): string {
	return scheduleKinds.find((k) => k.kind === kind)?.label ?? kind.replaceAll('_', ' ');
}

/** The zone the browser reports. Privacy modes report a stand-in such as Atlantic/Reykjavik. */
export function browserZone(): string {
	return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

/** The viewer's timezone: the one chosen on their account, or else the browser's. */
export function viewerZone(): string {
	return session.user?.timezone || browserZone();
}

const dateOrderLocale = { mdy: 'en-US', dmy: 'en-GB', ymd: 'en-SE' } as const;

/**
 * An instant as the viewer reads it: in their zone, hour cycle and date order, each the one
 * chosen on their account or else the browser's. options default to the locale's date and time;
 * a timeZone among them replaces the viewer's. format replaces the account's hour cycle and date
 * order.
 */
export function formatInstant(
	value: string | number | Date,
	options: Intl.DateTimeFormatOptions = {},
	format: Pick<User, 'hour_cycle' | 'date_order'> | null = session.user
): string {
	const order = format?.date_order;
	const hourCycle =
		format?.hour_cycle ||
		new Intl.DateTimeFormat(undefined, { hour: 'numeric' }).resolvedOptions().hourCycle;
	return new Date(value).toLocaleString(order ? dateOrderLocale[order] : undefined, {
		timeZone: viewerZone(),
		hourCycle,
		...options
	});
}

/** An instant as a date and time on the clock of timeZone. */
export function inZone(iso: string, timeZone: string): string {
	return formatInstant(iso, { timeZone, dateStyle: 'medium', timeStyle: 'short' });
}

export const schedules = {
	list: () => api.get<Page<Schedule>>('/schedules'),
	create: (body: CreateSchedule) => api.post<Schedule>('/schedules', body),
	patch: (id: string, body: PatchSchedule) => api.patch<Schedule>(`/schedules/${id}`, body),
	remove: (id: string) => api.del<void>(`/schedules/${id}`)
};

/**
 * A local time window, in minutes after midnight, that a clock change skips (the clocks go
 * forward over it) or repeats (they go back over it). end may pass 1440 when it wraps.
 */
export interface ClockChange {
	kind: 'skipped' | 'repeated';
	start: number;
	end: number;
}

const MINUTE = 60_000;
const DAY = 86_400_000;

/** The UTC offset of timeZone at instant ms, in minutes. */
function offsetMinutes(format: Intl.DateTimeFormat, ms: number): number {
	const name = format.formatToParts(ms).find((p) => p.type === 'timeZoneName')?.value ?? 'GMT';
	const m = /^GMT([+-])(\d{2}):(\d{2})$/.exec(name);
	return m ? (m[1] === '-' ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3])) : 0;
}

/**
 * The windows the clock changes of timeZone skip or repeat over the year after from. The
 * daemon's cron never runs a time inside a skipped window and runs one inside a repeated
 * window twice. Empty for a zone without daylight saving.
 */
export function clockChanges(timeZone: string, from = Date.now()): ClockChange[] {
	const format = new Intl.DateTimeFormat('en-US', { timeZone, timeZoneName: 'longOffset' });
	const changes: ClockChange[] = [];
	let before = offsetMinutes(format, from);
	for (let day = 1; day <= 366; day++) {
		const after = offsetMinutes(format, from + day * DAY);
		if (after === before) continue;
		// The change lies within this day: narrow it to the minute.
		let lo = Math.floor((from + (day - 1) * DAY) / MINUTE);
		let hi = Math.floor((from + day * DAY) / MINUTE);
		while (hi - lo > 1) {
			const mid = Math.floor((lo + hi) / 2);
			if (offsetMinutes(format, mid * MINUTE) === before) lo = mid;
			else hi = mid;
		}
		// The local time the clock read, on the old offset, as it changed.
		const at = (((hi + before) % 1440) + 1440) % 1440;
		const shift = after - before;
		changes.push(
			shift > 0
				? { kind: 'skipped', start: at, end: at + shift }
				: { kind: 'repeated', start: at + shift, end: at }
		);
		before = after;
	}
	return changes.map((c) => (c.start < 0 ? { ...c, start: c.start + 1440, end: c.end + 1440 } : c));
}

/** Whether minute, a local time of day, falls inside change's window. */
export function inClockChange(minute: number, change: ClockChange): boolean {
	return (((minute - change.start) % 1440) + 1440) % 1440 < change.end - change.start;
}
