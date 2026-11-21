import { session } from '#lib/state/session.svelte.js';
import { api } from './client';
import type { Job, User } from './types';

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
	/** A chain's steps, in order. Null for every other kind. */
	steps: ChainStep[] | null;
	/** Whether an update test applies what it tested when every mod loaded and nobody is on.
	 * Null for every other kind. */
	apply: boolean | null;
}

/**
 * One step of a chain schedule: the console command or lifecycle action to run, or a wait before
 * the next step. The chain stops at the first step that fails.
 */
export interface ChainStep {
	type: ChainStepType;
	/** Only for `command`. */
	command?: string;
	/** Only for `wait`, and for `wait_for` as the longest it waits. */
	seconds?: number;
	/** Only for `wait_for`. */
	condition?: WaitCondition;
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
	steps?: ChainStep[];
	apply?: boolean;
}

export interface PatchSchedule {
	cron?: string;
	timezone?: string;
	enabled?: boolean;
	steps?: ChainStep[];
	wait_for_empty?: boolean;
	max_deferral_seconds?: number;
	unknown_players?: 'wait' | 'run';
	apply?: boolean;
}

/** How often the schedule builder runs a task. `custom` is an expression written by hand. */
export type Every = 'hours' | 'day' | 'days' | 'month' | 'custom';

/**
 * The schedule builder's controls. `atTime` and `startAt` are `HH:MM`, as an
 * `<input type="time">` gives them; the rest hold the values the controls offer.
 */
export interface CronForm {
	every: Every;
	atTime: string;
	startAt: string;
	everyHours: string;
	weekdays: string[];
	monthDay: string;
	custom: string;
}

/** A fresh copy of the builder's starting values. */
export function defaultCronForm(): CronForm {
	return {
		every: 'day',
		atTime: '04:00',
		startAt: '00:00',
		everyHours: '6',
		weekdays: ['0'],
		monthDay: '1',
		custom: '0 4 * * *'
	};
}

/** The hour steps on offer: only the divisors of 24, so runs stay evenly spaced across midnight. */
export const hourChoices = ['1', '2', '3', '4', '6', '8', '12'];

/** The days of the week, Monday first, with cron's values, where Sunday is 0. */
export const weekdayChoices = [
	{ value: '1', label: 'Monday' },
	{ value: '2', label: 'Tuesday' },
	{ value: '3', label: 'Wednesday' },
	{ value: '4', label: 'Thursday' },
	{ value: '5', label: 'Friday' },
	{ value: '6', label: 'Saturday' },
	{ value: '0', label: 'Sunday' }
];

/** The days of the month on offer. 29 to 31 are left out, as not every month has them. */
export const monthDays = Array.from({ length: 28 }, (_, i) => String(i + 1));

/** The first hour of the day an every-few-hours run fires at. */
export function firstHourOf(f: CronForm): number {
	return Number(f.startAt.slice(0, 2)) % Number(f.everyHours);
}

/** The expression the builder's controls describe, or '' when they describe none yet. */
export function buildCron(f: CronForm): string {
	const hh = f.atTime.slice(0, 2);
	const mm = f.atTime.slice(3, 5);
	if (f.every === 'hours') {
		const first = firstHourOf(f);
		return `${Number(f.startAt.slice(3, 5))} ${first === 0 ? '*' : first}/${f.everyHours} * * *`;
	}
	if (f.every === 'days') {
		if (f.weekdays.length === 0) return '';
		const values = [...f.weekdays].sort((a, b) => Number(a) - Number(b)).join(',');
		return `${mm} ${hh} * * ${f.weekdays.length === 7 ? '*' : values}`;
	}
	if (f.every === 'month') return `${mm} ${hh} ${f.monthDay} * *`;
	if (f.every === 'day') return `${mm} ${hh} * * *`;
	return f.custom.trim();
}

const clockTime = /^([01]\d|2[0-3]):[0-5]\d$/;

/** Whether every value in f is one the builder's controls offer. */
function offered(f: CronForm): boolean {
	return (
		clockTime.test(f.atTime) &&
		clockTime.test(f.startAt) &&
		hourChoices.includes(f.everyHours) &&
		monthDays.includes(f.monthDay) &&
		new Set(f.weekdays).size === f.weekdays.length &&
		f.weekdays.every((d) => weekdayChoices.some((c) => c.value === d))
	);
}

/**
 * The builder controls that write expr, or a hand-written expression when none do. Only an
 * expression buildCron writes back is read into the controls, so nothing is interpreted that
 * the builder could not have produced. A leading zero left off the minute or hour of a daily,
 * weekly or monthly expression does not matter.
 */
export function readCron(expr: string): Partial<CronForm> {
	const pad = (n: string) => n.padStart(2, '0');
	let read: Partial<CronForm> | null = null;
	let written = expr;
	let m: RegExpExecArray | null;
	if ((m = /^(\d{1,2}) (\d{1,2}) \* \* \*$/.exec(expr))) {
		read = { every: 'day', atTime: `${pad(m[2])}:${pad(m[1])}` };
	} else if ((m = /^(\d{1,2}) (\d{1,2}) \* \* (\d(?:,\d)*)$/.exec(expr))) {
		read = { every: 'days', atTime: `${pad(m[2])}:${pad(m[1])}`, weekdays: m[3].split(',') };
	} else if ((m = /^(\d{1,2}) (\d{1,2}) (\d{1,2}) \* \*$/.exec(expr))) {
		read = { every: 'month', atTime: `${pad(m[2])}:${pad(m[1])}`, monthDay: m[3] };
	} else if ((m = /^(\d{1,2}) (\*|\d{1,2})\/(\d{1,2}) \* \* \*$/.exec(expr))) {
		const first = m[2] === '*' ? '0' : m[2];
		read = { every: 'hours', everyHours: m[3], startAt: `${pad(first)}:${pad(m[1])}` };
	}
	if (read && read.every !== 'hours' && m)
		written = `${pad(m[1])} ${pad(m[2])}${expr.slice(m[1].length + m[2].length + 1)}`;
	const form = { ...defaultCronForm(), ...read };
	if (read && offered(form) && buildCron(form) === written) return read;
	return { every: 'custom', custom: expr };
}

/** A list of words joined as prose: "a", "a and b", "a, b and c". */
function joinWords(words: string[]): string {
	return words.length < 2 ? words.join('') : `${words.slice(0, -1).join(', ')} and ${words.at(-1)}`;
}

/** A day of the month with its ordinal suffix: 1st, 2nd, 11th, 22nd. */
export function ordinal(n: number): string {
	const tens = n % 100;
	const suffix = tens >= 11 && tens <= 13 ? 'th' : (['th', 'st', 'nd', 'rd'][n % 10] ?? 'th');
	return `${n}${suffix}`;
}

/** What the builder's controls schedule, in words. It is empty for a written expression, which
 * only the daemon can interpret. */
export function describeCron(f: CronForm): string {
	if (f.every === 'hours') {
		const n = Number(f.everyHours);
		const times = Array.from(
			{ length: 24 / n },
			(_, i) => `${String(firstHourOf(f) + i * n).padStart(2, '0')}:${f.startAt.slice(3, 5)}`
		);
		return `Every ${n} hour${n === 1 ? '' : 's'} — ${times.join(', ')}`;
	}
	if (f.every === 'days') {
		const days = weekdayChoices.filter((d) => f.weekdays.includes(d.value));
		if (days.length === 0) return 'Choose at least one day';
		if (days.length === 7) return `Every day at ${f.atTime}`;
		return `Every ${joinWords(days.map((d) => d.label))} at ${f.atTime}`;
	}
	if (f.every === 'month')
		return `Every month on the ${ordinal(Number(f.monthDay))} at ${f.atTime}`;
	if (f.every === 'day') return `Every day at ${f.atTime}`;
	return '';
}

/** A stored expression in words, or '' when it is not one the builder writes. */
export function describeExpression(expr: string): string {
	const read = readCron(expr);
	return read.every === 'custom' ? '' : describeCron({ ...defaultCronForm(), ...read });
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
	{ kind: 'game_update', action: 'instance.update', label: 'Update the game', playerAware: false },
	{
		kind: 'update_test',
		action: 'instance.update',
		label: 'Test updates on a copy',
		playerAware: false
	},
	// A chain is offered to anyone who may stop the server; each step is then checked against
	// the action it needs, and a chain is player aware only when one of its steps stops the server.
	{ kind: 'chain', action: 'instance.stop', label: 'Run a sequence of steps', playerAware: false }
] as const;

/**
 * The steps a chain offers, each with the capability that gates it. The daemon holds the
 * authoritative set and checks every step on write; `action: null` needs none.
 */
export const chainSteps = [
	{ type: 'command', action: 'commands.send', label: 'Send a console command', stops: false },
	{ type: 'wait', action: null, label: 'Wait', stops: false },
	{ type: 'wait_for', action: null, label: 'Wait for the server', stops: false },
	{ type: 'backup', action: 'backups.create', label: 'Back up the world', stops: true },
	{ type: 'stop', action: 'instance.stop', label: 'Stop the server', stops: true },
	{ type: 'start', action: 'instance.start', label: 'Start the server', stops: false },
	{ type: 'restart', action: 'instance.restart', label: 'Restart the server', stops: true },
	{ type: 'game_update', action: 'instance.update', label: 'Update the game', stops: true }
] as const;

export type ChainStepType = (typeof chainSteps)[number]['type'];

/** What a `wait_for` step waits for. */
export const waitConditions = [
	{ value: 'ready', label: 'to be ready' },
	{ value: 'empty', label: 'to have no players' }
] as const;

export type WaitCondition = (typeof waitConditions)[number]['value'];

/** The most steps a chain holds, and the longest single wait, as the daemon enforces them. */
export const maxChainSteps = 20;
export const maxWaitSeconds = 3600;

/** Whether a chain with these steps takes a running server down, so it may wait for players. */
export function stopsServer(steps: ChainStep[]): boolean {
	return steps.some((s) => chainSteps.find((c) => c.type === s.type)?.stops);
}

/** A wait in words: "30 seconds", "5 minutes", "1 hour". */
export function waitWords(seconds: number): string {
	const [n, unit] =
		seconds % 3600 === 0
			? [seconds / 3600, 'hour']
			: seconds % 60 === 0
				? [seconds / 60, 'minute']
				: [seconds, 'second'];
	return `${n} ${unit}${n === 1 ? '' : 's'}`;
}

const stepWords: Record<Exclude<ChainStepType, 'command' | 'wait' | 'wait_for'>, string> = {
	backup: 'back up',
	stop: 'stop',
	start: 'start',
	restart: 'restart',
	game_update: 'update the game'
};

/** A chain in one line: `send "save" → wait 5 minutes → stop → start`. */
export function describeSteps(steps: ChainStep[]): string {
	return steps
		.map((s) =>
			s.type === 'command'
				? `send "${s.command ?? ''}"`
				: s.type === 'wait'
					? `wait ${waitWords(s.seconds ?? 0)}`
					: s.type === 'wait_for'
						? `wait up to ${waitWords(s.seconds ?? 0)} for the server ${s.condition === 'empty' ? 'to have no players' : 'to be ready'}`
						: stepWords[s.type]
		)
		.join(' → ');
}

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

/** A numeric date, which keeps the account's date order, and a time without seconds. */
const minuteFields: Intl.DateTimeFormatOptions = {
	year: 'numeric',
	month: 'numeric',
	day: 'numeric',
	hour: 'numeric',
	minute: '2-digit'
};

/**
 * An instant as the viewer reads it: in their zone, hour cycle and date order, each the one
 * chosen on their account or else the browser's. It is a numeric date and a time to the minute
 * unless options name a style or any date or time field down to the minute, which then decide
 * what is shown. A timeZone among them replaces the viewer's. format replaces the account's
 * hour cycle and date order.
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
	const chosen = ['dateStyle', 'timeStyle', ...Object.keys(minuteFields)].some(
		(field) => field in options
	);
	return new Date(value).toLocaleString(order ? dateOrderLocale[order] : undefined, {
		timeZone: viewerZone(),
		hourCycle,
		...(chosen ? {} : minuteFields),
		...options
	});
}

/** An instant as a date and time on the clock of timeZone. */
export function inZone(iso: string, timeZone: string): string {
	return formatInstant(iso, { timeZone });
}

export const schedules = {
	list: () => api.get<Page<Schedule>>('/schedules'),
	create: (body: CreateSchedule) => api.post<Schedule>('/schedules', body),
	patch: (id: string, body: PatchSchedule) => api.patch<Schedule>(`/schedules/${id}`, body),
	remove: (id: string) => api.del<void>(`/schedules/${id}`),
	/** Runs a server schedule now, as its clock would, and answers with the job it started. */
	run: (id: string) => api.post<Job>(`/schedules/${id}/run`)
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
 * The windows the clock changes of timeZone skip or repeat over the year after from, each
 * once. The scan runs 371 days because a weekday rule such as the last Sunday of October can
 * fall that long after its previous date. The daemon's cron never runs a time inside a
 * skipped window and runs one inside a repeated window twice. Empty for a zone without
 * daylight saving.
 */
export function clockChanges(timeZone: string, from = Date.now()): ClockChange[] {
	const format = new Intl.DateTimeFormat('en-US', { timeZone, timeZoneName: 'longOffset' });
	const changes: ClockChange[] = [];
	let before = offsetMinutes(format, from);
	for (let day = 1; day <= 371; day++) {
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
	return changes
		.map((c) => (c.start < 0 ? { ...c, start: c.start + 1440, end: c.end + 1440 } : c))
		.filter(
			(c, i, all) =>
				all.findIndex((o) => o.kind === c.kind && o.start === c.start && o.end === c.end) === i
		);
}

/** Whether minute, a local time of day, falls inside change's window. */
export function inClockChange(minute: number, change: ClockChange): boolean {
	return (((minute - change.start) % 1440) + 1440) % 1440 < change.end - change.start;
}
