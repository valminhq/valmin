import { api } from './client';

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

/** The viewer's own timezone, as the browser reports it. */
export function viewerZone(): string {
	return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

/** An instant as a date and time on the clock of timeZone. */
export function inZone(iso: string, timeZone: string): string {
	return new Date(iso).toLocaleString(undefined, {
		timeZone,
		dateStyle: 'medium',
		timeStyle: 'short'
	});
}

export const schedules = {
	list: () => api.get<Page<Schedule>>('/schedules'),
	create: (body: CreateSchedule) => api.post<Schedule>('/schedules', body),
	patch: (id: string, body: PatchSchedule) => api.patch<Schedule>(`/schedules/${id}`, body),
	remove: (id: string) => api.del<void>(`/schedules/${id}`)
};
