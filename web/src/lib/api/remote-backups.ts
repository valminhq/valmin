import { api } from './client';
import type { Job } from './types';

export interface RemoteSummary {
	destination_id: string | null;
	enabled: boolean;
	last_success_at: string | null;
	last_archive_at: string | null;
	last_consistent_archive_at: string | null;
	pending: number;
	failed: number;
	cleanup_pending: number;
}
export interface RemoteDestination {
	id: string;
	kind: 'webdav' | 'rclone';
	enabled: boolean;
	endpoint: string;
	username: string;
	remote_name: string;
	folder: string;
	has_credentials: boolean;
	last_test_at: string | null;
	last_test_error: string;
	summary: RemoteSummary;
}
export interface DestinationInput {
	kind: 'webdav' | 'rclone';
	enabled: boolean;
	endpoint: string;
	username: string;
	password?: string;
	remote_name: string;
	folder: string;
}
export interface RemoteCopy {
	id: string;
	destination_id: string;
	backup_id: string;
	instance_id: string;
	world_name: string;
	consistent: boolean;
	archive_created_at: string;
	status: 'pending' | 'uploading' | 'retry_wait' | 'succeeded' | 'failed' | 'cancelled' | 'pruned';
	attempts: number;
	next_attempt_at: string;
	deadline_at: string;
	last_error: string;
	succeeded_at: string | null;
	job_id: string | null;
	cleanup_pending: boolean;
	cleanup_error: string;
	source_available: boolean;
}
export interface RemoteCopyPage {
	items: RemoteCopy[];
	next_cursor: string | null;
	summary: RemoteSummary;
}
interface AcceptedCopy {
	copy_id: string;
	status: RemoteCopy['status'];
	job_id: string | null;
}
export type RcloneProvider = 'drive' | 'dropbox' | 'onedrive' | 'mega' | 's3';
/** What starts a connection setup. Only the chosen provider's fields are sent. */
export interface RcloneSetupInput {
	provider: RcloneProvider;
	name: string;
	client_id?: string;
	client_secret?: string;
	user?: string;
	password?: string;
	s3_provider?: string;
	endpoint?: string;
	region?: string;
	access_key_id?: string;
	secret_access_key?: string;
	session_token?: string;
}
export interface RcloneDrive {
	id: string;
	name: string;
	type: string;
}
/** A connection setup in progress. It never carries a credential or token. */
export interface RcloneSetup {
	id: string;
	provider: RcloneProvider;
	name: string;
	step: 'authorize' | 'choose_drive' | 'ready';
	callback_url: string;
	authorization_url?: string;
	drives?: RcloneDrive[];
	expires_at: string;
}
/** Either the provider's authorization callback or the chosen OneDrive drive. */
export interface RcloneSetupAdvance {
	state?: string;
	code?: string;
	error?: string;
	drive_id?: string;
}
const setupsPath = '/admin/rclone-setups';
export const rcloneSetups = {
	start: (body: RcloneSetupInput) => api.post<RcloneSetup>(setupsPath, body),
	get: (id: string) => api.get<RcloneSetup>(`${setupsPath}/${encodeURIComponent(id)}`),
	advance: (id: string, body: RcloneSetupAdvance) =>
		api.post<RcloneSetup>(`${setupsPath}/${encodeURIComponent(id)}/advance`, body),
	cancel: (id: string) => api.del<void>(`${setupsPath}/${encodeURIComponent(id)}`),
	finish: (id: string) => api.post<Job>(`${setupsPath}/${encodeURIComponent(id)}/finish`)
};

const destinationPath = '/admin/remote-backup-destination';
export const remoteBackups = {
	destination: () => api.get<RemoteDestination | null>(destinationPath),
	save: (body: DestinationInput) => api.put<RemoteDestination>(destinationPath, body),
	test: () => api.post<Job>(destinationPath + '/test'),
	remotes: () => api.get<{ items: string[] }>(destinationPath + '/remotes'),
	list: (id: string, cursor: string | null = null) =>
		api.get<RemoteCopyPage>(
			`/instances/${id}/remote-copies${cursor ? '?cursor=' + encodeURIComponent(cursor) : ''}`
		),
	upload: (id: string, backupId: string) =>
		api.post<AcceptedCopy>(`/instances/${id}/backups/${backupId}/remote-copy`),
	retry: (id: string, copyId: string) =>
		api.post<AcceptedCopy>(`/instances/${id}/remote-copies/${copyId}/retry`),
	cancel: (id: string, copyId: string) =>
		api.post<void>(`/instances/${id}/remote-copies/${copyId}/cancel`)
};
