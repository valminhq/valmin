import { api } from './client';
import type { Job } from './types';

/** One link of an instance's definition chain. `job_id` is present once the step landed, so
 * the steps before the cursor are also the audit trail of what built this instance. */
export interface OperationStep {
	kind: string;
	ref?: string;
	job_id?: string;
}

/**
 * An instance's outstanding definition chain (Q52). `cursor` is the index of the step still
 * owed: everything before it completed, everything from it on has not run.
 *
 * `running` means a job is carrying the chain forward; `interrupted` means the daemon stopped
 * while it was mid-chain and nothing will continue it without being asked.
 */
export interface Operation {
	id: string;
	kind: 'create' | 'import';
	state: 'running' | 'interrupted';
	cursor: number;
	steps: OperationStep[];
	created_at: string;
	updated_at: string;
}

export const operations = {
	/** Null when the instance owes nothing. A 404 here means the instance itself is not
	 * visible (ADR-038), so it is left to propagate. */
	get: (id: string) => api.get<Operation | null>(`/instances/${id}/operation`),
	/** Submits the outstanding step and returns its job. A second call while the first is still
	 * running is `409 job_in_progress`, not a second run (`11 §3`). */
	resume: (id: string) => api.post<Job>(`/instances/${id}/operation/resume`),
	/** Drops the steps that never ran. Nothing already installed or written is undone. */
	abandon: (id: string) => api.post<Operation>(`/instances/${id}/operation/abandon`)
};
