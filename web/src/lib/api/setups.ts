import { api, request } from './client';
import type { Job } from './types';
import type { ManifestConfig, ManifestLaunch } from './manifest';
import type { InstalledMod } from './mods';

export interface LinkedWorldBackup {
	id: string;
	world_name: string;
	created_at: string;
	consistent: boolean;
	available?: boolean;
}

export interface SavedSetup {
	id: string;
	name: string;
	created_at: string;
	game_build_id: string;
	world_name: string;
	world_backup?: LinkedWorldBackup | null;
}

export type SetupMod = Pick<
	InstalledMod,
	'full_name' | 'source' | 'version' | 'side' | 'enabled' | 'locked' | 'installed_as'
>;

export interface SetupDetail extends SavedSetup {
	instance: ManifestLaunch;
	mods: SetupMod[];
	configs: ManifestConfig[];
}

export interface SetupPreview {
	etag: string;
	ready: boolean;
	problems: string[];
	current: {
		build: string;
		launch: ManifestLaunch;
		mods: SetupMod[];
		configs: ManifestConfig[];
	};
	setup: SetupDetail;
}

const path = (instanceId: string, setupId?: string) =>
	`/instances/${encodeURIComponent(instanceId)}/setups${setupId ? `/${encodeURIComponent(setupId)}` : ''}`;

export const setups = {
	list: (instanceId: string) => api.get<{ items: SavedSetup[] }>(path(instanceId)),
	save: (instanceId: string, name: string, worldBackupId?: string) =>
		api.post<Job>(path(instanceId), {
			name,
			...(worldBackupId ? { world_backup_id: worldBackupId } : {})
		}),
	get: (instanceId: string, setupId: string) => api.get<SetupDetail>(path(instanceId, setupId)),
	preview: (instanceId: string, setupId: string) =>
		api.get<SetupPreview>(`${path(instanceId, setupId)}/preview`),
	restore: (instanceId: string, setupId: string, etag: string) => {
		if (!etag) throw new Error('Restoring a setup needs its preview fingerprint.');
		return request<Job>(`${path(instanceId, setupId)}/restore`, {
			method: 'POST',
			headers: { 'If-Match': etag }
		});
	},
	remove: (instanceId: string, setupId: string) =>
		request<Job>(path(instanceId, setupId), { method: 'DELETE' })
};
