import { api } from './client';
import type { Job } from './types';

/** One package in the cached index, as `GET /mods/search` and `GET /mods/{ns}/{name}`
 * serve it (`04 §3`). The panel searches its own copy — the browser never reaches
 * Thunderstore — so `synced_at` on the page below is how stale this row is. */
export interface ModSummary {
	full_name: string;
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

export interface ModSearchPage {
	items: ModSummary[];
	next_cursor: string | null;
	/** When the index was last refreshed, or null before the first sync has ever run. */
	synced_at: string | null;
}

/** `↯` Two answers, not three. Null is the third state and it is deliberately absent from
 * this union: it means the panel has nothing to compare against — no load report yet, or a
 * package that places nothing loadable — which is not the same claim as `not_seen`. There
 * is no `failed` either; nobody has measured what a failure looks like (Q38). */
export type LoadStatus = 'loaded' | 'not_seen';

export interface InstalledMod {
	full_name: string;
	/** Author and display name, from the daemon's catalogue. Empty when it holds no row for
	 * the package — never synced, or removed upstream — in which case `full_name` is all
	 * there is to show. The panel does not split the ident here; a hyphen is legal inside
	 * either half, so only the daemon knows where the boundary is. */
	namespace: string;
	name: string;
	version: string;
	/** `explicit` if somebody asked for it, `dependency` if a closure pulled it in. */
	installed_as: string;
	side: string;
	enabled: boolean;
	installed_at: string;
	file_count: number;
	load_status: LoadStatus | null;
}

/** What the mod loader reported the last time this server started (`04 §3`). Null when
 * there is no report to read. */
export interface PluginLoad {
	observed_at: string;
	/** The count the loader announced, null when it announced none. */
	declared: number | null;
	loaded: number;
	/** `↯` The panel's own sentence when the two numbers disagree, rendered as sent. The
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
	version: string;
	transitive: boolean;
	no_op: boolean;
}

export interface ResolveResult {
	nodes: ResolvedNode[];
}

export const mods = {
	search: (q: string, cursor: string | null = null) => {
		const params = new URLSearchParams();
		if (q) params.set('q', q);
		if (cursor) params.set('cursor', cursor);
		const query = params.toString();
		return api.get<ModSearchPage>(`/mods/search${query ? `?${query}` : ''}`);
	},
	installed: (id: string) => api.get<InstalledMods>(`/instances/${id}/mods`),

	/**
	 * One package's catalogue row — what the index knows about a mod, which the installed
	 * list does not carry (Q39): its current version, and whether its author has deprecated
	 * it. The response also carries the full version history, which no screen needs yet.
	 *
	 * `↯` The path splits `Namespace-Name` at the **first** hyphen, mirroring the route the
	 * daemon serves and `03 §6.2`'s own notation. This is package-index addressing, not game
	 * knowledge — and a wrong split resolves to a 404, never to another package, because
	 * `full_name` is the primary key on the other side.
	 */
	/** `↯` Takes the two halves, never a `Namespace-Name` ident to split. A hyphen is legal
	 * inside either half (`03 §6.2`), so the boundary is not recoverable from the joined
	 * string — the daemon carries both halves on every row that needs them. */
	detail: (namespace: string, name: string) =>
		api.get<ModSummary>(`/mods/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`),

	/** `↯` The dry run, and it is not an optimisation — `04 §3` puts it before install on
	 * purpose so the closure is confirmed before anything downloads or is written. */
	resolve: (id: string, fullName: string, version: string) =>
		api.post<ResolveResult>(`/instances/${id}/mods/resolve`, {
			full_name: fullName,
			version
		}),

	// Both of these answer a job, never the resource (ADR-028, `11 §3`).
	install: (id: string, fullName: string, version: string) =>
		api.post<Job>(`/instances/${id}/mods`, { full_name: fullName, version }),
	uninstall: (id: string, fullName: string, removeOrphans: boolean) =>
		api.del<Job>(
			`/instances/${id}/mods/${encodeURIComponent(fullName)}?remove_orphans=${removeOrphans}`
		)
};
