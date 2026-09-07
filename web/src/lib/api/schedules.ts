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
	/** The location the expression is evaluated in, sent by the daemon so a screen reading
	 * "03:00" never leaves an operator to assume it means local time. */
	timezone: string;
}

export interface CreateSchedule {
	instance_id?: string | null;
	kind: string;
	cron: string;
	enabled?: boolean;
}

export interface PatchSchedule {
	cron?: string;
	enabled?: boolean;
}

interface Page<T> {
	items: T[];
	next_cursor: string | null;
}

/**
 * The kinds this screen offers for one instance, each with the capability that gates it. The
 * daemon holds the authoritative set and refuses anything else (`12 §3.1`); this is the label
 * and the gate, which no endpoint sends. The global kinds — `update_check` and `prune` — are
 * not here: they belong to no instance and need `schedules.global`.
 */
export const scheduleKinds = [
	{ kind: 'backup', action: 'backups.create', label: 'Back up this server' },
	{ kind: 'restart', action: 'instance.restart', label: 'Restart this server' },
	{ kind: 'game_update', action: 'instance.update', label: 'Update the game' }
] as const;

export const schedules = {
	list: () => api.get<Page<Schedule>>('/schedules').then((p) => p.items),
	create: (body: CreateSchedule) => api.post<Schedule>('/schedules', body),
	patch: (id: string, body: PatchSchedule) => api.patch<Schedule>(`/schedules/${id}`, body),
	remove: (id: string) => api.del<void>(`/schedules/${id}`)
};
