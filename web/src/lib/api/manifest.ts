import { api } from './client';
import type { Job } from './types';
import type { ModSide } from './mods';

/** The launch half of an instance's definition (`04 §3`). No id, no port, no password: those
 * identify one installation, and a manifest is meant to travel. */
export interface ManifestLaunch {
	server_name: string;
	world_name: string;
	public: boolean;
	crossplay: boolean;
	preset?: string;
	modifiers?: Record<string, string>;
	extra_args?: string;
	mem_limit_mb: number;
	cpu_limit: number | null;
	backup_keep_cold: number;
	backup_keep_hot: number;
	backup_on_restart: boolean;
}

export interface ManifestMod {
	full_name: string;
	version: string;
	side?: ModSide;
}

/** One `.cfg` file, whole. `file` is a bare filename — the daemon refuses anything else. */
export interface ManifestConfig {
	file: string;
	content: string;
}

/** `01` G6's definition of one instance. `schema` is an integer, and a value this panel does
 * not know is refused rather than half-read. */
export interface InstanceManifest {
	schema: number;
	name: string;
	instance: ManifestLaunch;
	mods: ManifestMod[];
	configs: ManifestConfig[];
}

/** What an import would do, reported before any of it happens. `available` false means the
 * catalogue can no longer supply that exact version, which fails the import — a manifest that
 * quietly installed a different version would not be a manifest. */
export interface ManifestPreview {
	name: string;
	instance: ManifestLaunch;
	mods: Array<ManifestMod & { available: boolean }>;
	configs: Array<{ file: string; size_bytes: number }>;
	problems: Array<{ field: string; detail: string }>;
}

export const manifest = {
	/** Needs `instance.settings`, `mods.list` and `config.read` together — the document is a
	 * projection of all three. */
	export: (id: string) => api.get<InstanceManifest>(`/instances/${id}/manifest`),
	preview: (m: InstanceManifest) =>
		api.post<ManifestPreview>('/instances/manifest/preview', { manifest: m }),
	/** Returns a job, never the instance (ADR-028). Name and password are supplied here, never
	 * read from the file. */
	import: (m: InstanceManifest, name: string, password: string, start: boolean) =>
		api.post<Job>('/instances/import', {
			manifest: m,
			name,
			password,
			start_after_provision: start
		})
};
