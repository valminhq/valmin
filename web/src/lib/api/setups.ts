import { api, request } from './client';
import type { Job } from './types';
import type { ManifestLaunch } from './manifest';
import type { InstalledMod } from './mods';

export interface LinkedWorldBackup {
	id: string;
	world_name: string;
	created_at: string;
	consistent: boolean;
	available?: boolean;
	size_bytes?: number;
}

export interface SavedSetup {
	id: string;
	name: string;
	created_at: string;
	/** Who saved it, or whose change an automatic one comes before; null for the panel's own. */
	created_by: string | null;
	game_build_id: string;
	world_name: string;
	world_backup?: LinkedWorldBackup | null;
	/** False for a setup saved before setups held the whole config tree: it restores only
	 * the `.cfg` files directly in `BepInEx/config`. */
	config_tree: boolean;
	/** The config tree held files past the listing limits, which the setup left out. */
	configs_truncated: boolean;
	/** Saved by the panel before a restore or a mod change. */
	automatic: boolean;
	/** Keeps only the packages their registry archives cannot reproduce; the rest are
	 * downloaded on restore. */
	light: boolean;
	/** Waiting to be linked to the world backup the next mod change takes, if its mods are
	 * still the server's then. */
	world_pending: boolean;
}

/** How a setup is saved: which world backup to link, to take one now, or to wait for the one
 * the next mod change takes, and whether to keep every package. */
export interface SetupSaveOptions {
	worldBackupId?: string;
	backUpWorld?: boolean;
	linkNextWorld?: boolean;
	light?: boolean;
}

/** One config file of a setup or of the current server, by path under `BepInEx/config`.
 * `content` is sent for a top-level `.cfg` file only. */
export interface SetupConfig {
	file: string;
	content?: string;
	sha256: string;
	size_bytes: number;
}

export type SetupMod = Pick<
	InstalledMod,
	'full_name' | 'source' | 'version' | 'side' | 'enabled' | 'locked' | 'installed_as'
>;

export interface SetupDetail extends SavedSetup {
	instance: ManifestLaunch;
	mods: SetupMod[];
	configs: SetupConfig[];
}

export interface SetupPreview {
	etag: string;
	ready: boolean;
	problems: string[];
	current: {
		build: string;
		launch: ManifestLaunch;
		mods: SetupMod[];
		configs: SetupConfig[];
	};
	setup: SetupDetail;
}

const path = (instanceId: string, setupId?: string) =>
	`/instances/${encodeURIComponent(instanceId)}/setups${setupId ? `/${encodeURIComponent(setupId)}` : ''}`;

export const setups = {
	list: (instanceId: string) => api.get<{ items: SavedSetup[] }>(path(instanceId)),
	save: (instanceId: string, name: string, options: SetupSaveOptions = {}) =>
		api.post<Job>(path(instanceId), {
			name,
			...(options.worldBackupId ? { world_backup_id: options.worldBackupId } : {}),
			...(options.backUpWorld ? { back_up_world: true } : {}),
			...(options.linkNextWorld ? { link_next_world_backup: true } : {}),
			...(options.light ? { light: true } : {})
		}),
	get: (instanceId: string, setupId: string) => api.get<SetupDetail>(path(instanceId, setupId)),
	/** The saved text of one config file of a setup. */
	config: (instanceId: string, setupId: string, file: string) =>
		api
			.getText(`${path(instanceId, setupId)}/configs/${encodeURIComponent(file)}`)
			.then((res) => res.text),
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
