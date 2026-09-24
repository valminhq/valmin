import { api } from './client';
import type { Job } from './types';

/** Which registry a listing came from (`03 §6.1`). A package both carry appears as one row
 * per registry, each with its own versions, because they can serve different bytes under one
 * ident (B14). */
export type ModSource = 'thunderstore' | 'hexium';

/** The registries, in the order the switch offers them. `null` is "all", which is the
 * absence of a filter rather than a third registry. */
export const modSources: ReadonlyArray<{ value: ModSource | null; label: string }> = [
	{ value: null, label: 'All' },
	{ value: 'thunderstore', label: 'Thunderstore' },
	{ value: 'hexium', label: 'Hexium' }
];

/** What to call a registry, and how to mark it.
 *
 * Inline palette classes in the house pattern (`state-badge.svelte`): `app.css` is vendored
 * shadcn output (ADR-002) and gains no tokens of its own. Both mod screens read these rather
 * than spelling the classes out, so the two cannot drift.
 *
 * Colour is never the only signal — every screen that colours a version also renders the
 * registry's name — so the distinction survives a colour-blind operator and a greyscale
 * screenshot. A registry the daemon knows and this build does not reads as undefined, so
 * every call site falls back to unstyled rather than to `undefined` in a class list. */
export const sourceLabel: Record<ModSource, string> = {
	thunderstore: 'Thunderstore',
	hexium: 'Hexium'
};

export const sourceText: Record<ModSource, string> = {
	thunderstore: 'text-sky-700 dark:text-sky-300',
	hexium: 'text-amber-700 dark:text-amber-300'
};

export const sourceBadge: Record<ModSource, string> = {
	thunderstore: 'bg-sky-100 text-sky-700 dark:bg-sky-950 dark:text-sky-300',
	hexium: 'bg-amber-100 text-amber-700 dark:bg-amber-950 dark:text-amber-300'
};

/** One package in the cached index, as `GET /mods/search` and `GET /mods/{ns}/{name}`
 * serve it (`04 §3`). The panel searches its own copy — the browser never reaches a
 * registry — so `synced_at` on the page below is how stale this row is. */
export interface ModSummary {
	full_name: string;
	source: ModSource;
	namespace: string;
	name: string;
	description: string;
	latest_version: string;
	downloads: number;
	rating: number;
	is_deprecated: boolean;
	categories: string[];
	icon_url: string;
}

export interface RegistryStatus {
	source: ModSource;
	enabled: boolean;
	synced_at: string | null;
}

export type ModInstallTarget = Pick<
	ModSummary,
	'full_name' | 'source' | 'name' | 'latest_version' | 'is_deprecated'
>;

export interface ModSearchPage {
	registries: RegistryStatus[];
	items: ModSummary[];
	next_cursor: string | null;
	/** When the index was last refreshed, or null before the first sync has ever run. */
	synced_at: string | null;
}

/** Null is a fourth state and it is deliberately absent from this union: it means the
 * panel has nothing to compare against — no load report yet, or a package that places
 * nothing loadable — which is not the same claim as `not_seen`. `failed` is a plugin the
 * loader said it could not load, with its own line in `load_error` (Q38); `not_seen` is one
 * it said nothing about. */
export type LoadStatus = 'loaded' | 'not_seen' | 'failed';

/** The curator-supplied compatibility labels accepted by the daemon. */
export type ModSide = 'server_only' | 'client_required' | 'client_optional' | 'unknown';

export const modSides: ReadonlyArray<{ value: ModSide; label: string }> = [
	{ value: 'unknown', label: 'Unknown' },
	{ value: 'server_only', label: 'Server only' },
	{ value: 'client_required', label: 'Client required' },
	{ value: 'client_optional', label: 'Client optional' }
];

export interface InstalledMod {
	full_name: string;
	/** The registry the installed files came from. Recorded at install and never re-derived,
	 * so an update is compared against the same registry the files are already from. */
	source: ModSource;
	/** Author and display name, from the daemon's catalogue. Empty when it holds no row for
	 * the package — never synced, or removed upstream — in which case `full_name` is all
	 * there is to show. The panel does not split the ident here; a hyphen is legal inside
	 * either half, so only the daemon knows where the boundary is. */
	namespace: string;
	name: string;
	version: string;
	/** `explicit` if somebody asked for it, `dependency` if a closure pulled it in. */
	installed_as: string;
	/** A newer version from the enabled, installed registry, or an empty string. */
	update_version: string;
	is_deprecated: boolean;
	side: ModSide;
	enabled: boolean;
	installed_at: string;
	file_count: number;
	load_status: LoadStatus | null;
	/** The loader's own line naming the failure, set only when `load_status` is `failed`.
	 * Rendered as sent: the wording is the loader's, not the panel's. */
	load_error: string | null;
}

/** What the mod loader reported the last time this server started (`04 §3`). Null when
 * there is no report to read. */
export interface PluginLoad {
	observed_at: string;
	/** The count the loader announced, null when it announced none. */
	declared: number | null;
	loaded: number;
	/** How many plugins the loader said it could not load. */
	failed: number;
	/** The panel's own sentence when the two numbers disagree, rendered as sent. The
	 * gap is reported, never resolved, and the wording belongs to the daemon — a frontend
	 * that composed it would be holding a piece of game knowledge (F2, `02 §2.1`). */
	discrepancy: string | null;
}

export interface InstalledMods {
	mods: InstalledMod[];
	plugin_load: PluginLoad | null;
}

/** One package in the closure a resolve previews. `transitive` came in as somebody else's
 * dependency; `no_op` is already installed at a version that satisfies the request. */
export interface ResolvedNode {
	full_name: string;
	/** Which registry this exact version resolved from. A dependency ident names no
	 * registry, so a closure can span both (ADR-213). */
	source: ModSource;
	version: string;
	transitive: boolean;
	no_op: boolean;
}

export interface ResolveResult {
	nodes: ResolvedNode[];
}

/** One package in the client manifest, or one the export left behind. `reason` is `tagged`
 * or `dependency` for a member, and the side tag itself for an exclusion. */
export interface ExportEntry {
	full_name: string;
	version: string;
	side: ModSide;
	reason: string;
}

/** Something a client needs that the export cannot supply: a dependency the admin marked
 * server-only, or one the catalogue cannot describe. */
export interface ExportConflict {
	full_name: string;
	version: string;
	required_by: string;
	side: ModSide;
}

/** `GET /instances/{id}/mods/export` (`04 §3`). The preview exists so what is missing from
 * the manifest is visible before the file is downloaded. */
export interface ExportPreview {
	profile_name: string;
	mods: ExportEntry[];
	excluded: ExportEntry[];
	conflicts: ExportConflict[];
}

/** One installed package and the version "Update all" moves it to. The preview lists these
 * and the apply request sends them back unchanged, so the job installs what was confirmed. */
export interface UpdateTarget {
	full_name: string;
	source: ModSource;
	from_version?: string;
	version: string;
}

/** One row of the combined diff. `from_version` is empty for a package the updates newly
 * pull in as a dependency. */
export interface UpdateNode {
	full_name: string;
	source: ModSource;
	from_version: string;
	version: string;
	transitive: boolean;
}

/** `POST /instances/{id}/mods/updates/resolve`: everything "Update all" would change (Q39). */
export interface UpdatePreview {
	targets: UpdateTarget[];
	nodes: UpdateNode[];
	/** Whether the world is archived before any file changes. False only for a server with
	 * no world yet. */
	backup: boolean;
}

export const mods = {
	/** `source` null searches every registry; naming one narrows to its listing. */
	search: (q: string, source: ModSource | null = null, cursor: string | null = null) => {
		const params = new URLSearchParams();
		if (q) params.set('q', q);
		if (source) params.set('source', source);
		if (cursor) params.set('cursor', cursor);
		const query = params.toString();
		return api.get<ModSearchPage>(`/mods/search${query ? `?${query}` : ''}`);
	},
	installed: (id: string) => api.get<InstalledMods>(`/instances/${id}/mods`),
	setSide: (id: string, fullName: string, side: ModSide) =>
		api.patch<InstalledMod>(`/instances/${id}/mods/${encodeURIComponent(fullName)}`, { side }),

	/**
	 * One package's catalogue row — what the index knows about a mod, which the installed
	 * list does not carry (Q39): its current version, and whether its author has deprecated
	 * it. The response also carries the full version history, which no screen needs yet.
	 *
	 * The path splits `Namespace-Name` at the first hyphen, mirroring the route the
	 * daemon serves and `03 §6.2`'s own notation. This is package-index addressing, not game
	 * knowledge — and a wrong split resolves to a 404, never to another package, because
	 * `full_name` is the primary key on the other side.
	 */
	/** Takes the two halves, never a `Namespace-Name` ident to split. A hyphen is legal
	 * inside either half (`03 §6.2`), so the boundary is not recoverable from the joined
	 * string — the daemon carries both halves on every row that needs them. */
	detail: (namespace: string, name: string, source: ModSource | null = null) =>
		api.get<ModSummary>(
			`/mods/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}` +
				(source ? `?source=${source}` : '')
		),

	/** The dry run, and it is not an optimisation — `04 §3` puts it before install on
	 * purpose so the closure is confirmed before anything downloads or is written. */
	resolve: (id: string, fullName: string, version: string, source: ModSource) =>
		api.post<ResolveResult>(`/instances/${id}/mods/resolve`, {
			full_name: fullName,
			version,
			source
		}),

	// Both of these answer a job, never the resource (ADR-028, `11 §3`).
	install: (id: string, fullName: string, version: string, source: ModSource) =>
		api.post<Job>(`/instances/${id}/mods`, { full_name: fullName, version, source }),
	/** The combined diff of every available update: a dry run, like `resolve`. */
	previewUpdates: (id: string) => api.post<UpdatePreview>(`/instances/${id}/mods/updates/resolve`),
	/** One job: archive the world, then apply the confirmed targets together. */
	applyUpdates: (id: string, targets: UpdateTarget[]) =>
		api.post<Job>(`/instances/${id}/mods/updates`, { targets }),
	uninstall: (id: string, fullName: string, removeOrphans: boolean) =>
		api.del<Job>(
			`/instances/${id}/mods/${encodeURIComponent(fullName)}?remove_orphans=${removeOrphans}`
		),

	/** What a client-side manifest would contain, and what it would leave out. */
	exportPreview: (id: string) => api.get<ExportPreview>(`/instances/${id}/mods/export`),
	/** The archive itself, followed as a link so the browser saves it. Refused with
	 * `409 mod_conflict` while the preview reports a conflict. */
	exportUrl: (id: string) => `/api/v1/instances/${id}/mods/export?format=r2z`
};
