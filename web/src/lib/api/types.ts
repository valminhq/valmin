// The wire shapes the shell needs. `↯` No Valheim in here (F2, `02 §2.1`): if the frontend
// ever needs to know what a `Single` is, the backend failed to send a `widget` field.

export type Role = 'admin' | 'member';

export interface User {
	id: string;
	username: string;
	role: Role;
	disabled: boolean;
	created_at: string;
	last_login_at: string | null;
}

export interface InstancePermissions {
	instance_id: string;
	/** `↯` The UI renders from this, never from `role` (F3, `09 §4.2`). */
	allowed_actions: string[];
}

export interface MyPermissions {
	user_id: string;
	/** `↯` Reported so an operator can see it, never so the UI can branch on it (F3). */
	role: Role;
	/** Global capabilities — `09 §3.3`'s never-grantable set for an admin, empty otherwise.
	 * This is what a "New server" button renders from: it belongs to no instance, so the
	 * per-instance list below cannot answer for it. */
	allowed_actions: string[];
	instances: InstancePermissions[];
}

export type JobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled';

/**
 * A job, as `11 §3`'s 202 stub and as the resource `GET /jobs/{id}` returns — the same
 * shape, grown.
 *
 * `↯` The identifier is `job_id` here and `id` on the socket's `job` message (`04 §4`). Two
 * spellings for one value is not a mistake to tidy: both are the specification, and the one
 * place that has to know is the reconciliation in jobs.ts.
 */
export interface Job {
	job_id: string;
	kind: string;
	status: JobStatus;
	instance_id?: string | null;
	progress: number;
	message?: string;
	error_code?: string;
	error?: string;
	clean?: boolean;
	created_at: string;
	started_at?: string;
	finished_at?: string;
}
