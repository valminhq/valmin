// The wire shapes the shell needs. No Valheim in here (F2, `02 §2.1`): if the frontend
// ever needs to know what a `Single` is, the backend failed to send a `widget` field.

export type Role = 'admin' | 'member';

/** The role that reaches every server without a grant. Used to describe listed accounts, never to gate the UI. */
export const adminRole: Role = 'admin';

export interface User {
	id: string;
	username: string;
	role: Role;
	disabled: boolean;
	/** The bootstrap account (`09 §2`). The panel refuses to demote, disable or delete it. */
	owner: boolean;
	created_at: string;
	last_login_at: string | null;
	/** The IANA zone the user reads times in, or '' to follow their browser. */
	timezone: string;
}

export interface InstancePermissions {
	instance_id: string;
	/** The UI renders from this, never from `role` (F3, `09 §4.2`). */
	allowed_actions: string[];
}

export interface MyPermissions {
	user_id: string;
	/** Reported so an operator can see it, never so the UI can branch on it (F3). */
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
 * The identifier is `job_id` here and `id` on the socket's `job` message (`04 §4`). Two
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
	/** The username of the person who started the job. Absent for system work and for a user
	 * who has since been deleted. */
	requested_by_name?: string;
	/** True when a schedule started the job. */
	scheduled?: boolean;
	/** The packages a mod job was asked to change, where its stored arguments say so. */
	changes?: JobChange[];
}

export type JobChangeAction = 'install' | 'update' | 'uninstall' | 'enable' | 'disable';

/** One package a mod job installs, updates, removes, enables or disables. The versions are
 * present only where the job recorded them. */
export interface JobChange {
	action: JobChangeAction;
	full_name: string;
	from_version?: string;
	to_version?: string;
}
