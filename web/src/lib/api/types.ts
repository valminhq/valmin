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
	role: Role;
	instances: InstancePermissions[];
}

/** The job stub every 202 carries (`11 §3`) — never the resource. */
export interface JobStub {
	job_id: string;
	kind: string;
	status: string;
	instance_id: string | null;
}

export interface Job {
	id: string;
	kind: string;
	status: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled';
	progress: number;
	message: string;
	error_code: string | null;
	error: string | null;
	instance_id: string | null;
}
