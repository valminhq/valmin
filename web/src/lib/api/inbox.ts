import { api } from './client';

/** One open operational condition. The daemon sends the facts; the copy is written here. */
export interface InboxItem {
	kind: InboxKind;
	severity: 'critical' | 'warning';
	instance_id: string | null;
	instance_name?: string;
	since_at: string;
	detail?: Record<string, string>;
}

export type InboxKind =
	| 'job_failed'
	| 'low_disk'
	| 'stale_backup'
	| 'unclean_stop'
	| 'restart_required'
	| 'update_available'
	| 'instance_error'
	| 'crash_loop'
	| 'job_stuck';

interface Page<T> {
	items: T[];
	next_cursor: string | null;
}

export const inbox = () => api.get<Page<InboxItem>>('/instances/inbox').then((p) => p.items);
