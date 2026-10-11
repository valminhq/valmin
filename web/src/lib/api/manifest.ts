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
	source?: string;
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
	/** `settings` is set for a code, whose configs are the changed settings alone. */
	configs: Array<{ file: string; size_bytes: number; settings?: TemplateSetting[] }>;
	problems: Array<{ field: string; detail: string }>;
}

/** One changed setting. `secret` marks a name that reads as a credential, which the code leaves
 * out unless its file's `secrets_included`; `suspicious` marks a value that reads as one. */
export interface TemplateSetting {
	key: string;
	value: string;
	secret: boolean;
	suspicious: boolean;
}

/** One config file's changed settings. `excluded` means the code leaves the whole file out. */
export interface TemplateConfig {
	file: string;
	excluded: boolean;
	secrets_included: boolean;
	settings: TemplateSetting[];
}

/** A server as a template code, with what the code leaves out. */
export interface TemplateCode {
	code: string;
	mods: number;
	settings: number;
	secrets_left_out: number;
	disabled_left_out: number;
	configs: TemplateConfig[];
}

/** Per-file choices for a template code, by config file name. */
export interface TemplateCodeChoices {
	exclude?: string[];
	includeSecrets?: string[];
}

/** What an import reads: a definition file's document, or a template code. */
export type ManifestSource = { manifest: InstanceManifest } | { code: string };

/** One template from a template repository. `source` is the repository's archive URL. */
export interface CatalogueTemplate {
	source: string;
	slug: string;
	name: string;
	description: string;
	mods: number;
	code: string;
}

/** Every configured template repository: its templates, the template files it could not use,
 * and when each repository was last read. */
export interface TemplateCatalogue {
	sources: Array<{ url: string; fetched_at: string | null; error: string | null }>;
	templates: CatalogueTemplate[];
	problems: Array<{ source: string; path: string; detail: string }>;
}

export const templates = {
	/** `refresh` reads every repository again instead of serving the panel's copy. */
	list: (refresh = false) =>
		api.get<TemplateCatalogue>(refresh ? '/templates?refresh=true' : '/templates')
};

export const manifest = {
	/** Needs `instance.settings`, `mods.list` and `config.read` together — the document is a
	 * projection of all three. */
	export: (id: string) => api.get<InstanceManifest>(`/instances/${id}/manifest`),
	/** The same capabilities as `export`. */
	code: (id: string, choices: TemplateCodeChoices = {}) => {
		const query = new URLSearchParams([
			...(choices.exclude ?? []).map((file) => ['exclude', file]),
			...(choices.includeSecrets ?? []).map((file) => ['include_secrets', file])
		]).toString();
		return api.get<TemplateCode>(`/instances/${id}/manifest/code${query ? `?${query}` : ''}`);
	},
	preview: (from: ManifestSource) => api.post<ManifestPreview>('/instances/manifest/preview', from),
	/** Returns a job, never the instance (ADR-028). Name and password are supplied here, never
	 * read from the file. */
	import: (from: ManifestSource, name: string, password: string, start: boolean) =>
		api.post<Job>('/instances/import', {
			...from,
			name,
			password,
			start_after_provision: start
		})
};
