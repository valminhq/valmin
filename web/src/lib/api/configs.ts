import { api } from './client';

/** What a setting can hold once the daemon has typed it (`11 §1.1`). */
export type ConfigValue = string | number | boolean;

/** One row of `GET /instances/{id}/configs`. `file` is the path relative to `dir`, and may
 * name a subdirectory. `plugin` comes from the file's own header and is empty when it has none. */
export interface ConfigFile {
	file: string;
	plugin: string;
	size_bytes: number;
	/** The installed mods this file belongs to, matched by name. Empty for a file no installed
	 * mod uses. */
	installed_mods: string[];
	/** The file's directory relative to the game installation; empty for a file an installed mod
	 * keeps at the installation root. */
	dir: string;
	/** Whether saved setups, definitions and template codes carry this file. */
	portable: boolean;
	/** The mod update whose version of this file replaced edits, while that is still its newest
	 * version. */
	mod_replaced_edits: ModReplaced | null;
}

export interface ModReplaced {
	mod: string;
	version: string;
	created_at: string;
}

export interface ConfigList {
	items: ConfigFile[];
	/** The daemon's sentence about why the list looks the way it does, rendered as sent:
	 * composing it here would need Valheim knowledge the SPA does not hold (F2). */
	note?: string;
}

export interface ConfigRange {
	min: number;
	max: number;
}

/**
 * One setting, as `04 §3` serves it.
 *
 * `widget` is the control to render, decided by the daemon (F2). `type` is shown to the
 * operator and never branched on here. `step` sizes a numeric control; zero means continuous.
 */
export interface ConfigSetting {
	key: string;
	type: string;
	description: string;
	default: ConfigValue;
	current: ConfigValue;
	range: ConfigRange | null;
	options: string[] | null;
	widget: string;
	step: number;
}

export interface ConfigSection {
	name: string;
	settings: ConfigSetting[];
}

export interface ConfigSchema {
	file: string;
	plugin: string;
	sections: ConfigSection[];
}

/**
 * A version of the file the panel kept, projected the same way the file itself is.
 * `captured_at` is how old the copy is, which nothing else says.
 */
export interface ConfigCopy extends ConfigSchema {
	captured_at: string;
}

/** What produced the bytes of a kept version. */
export type RevisionSource = 'found' | 'outside' | 'edit' | 'setup' | 'mod';

/**
 * One entry of a file's history, newest first. `user` is set for an edit; `mod` and the
 * versions for a mod install, update or downgrade. `baseline` marks the version template codes
 * measure changes against.
 */
export interface ConfigRevision {
	id: string;
	created_at: string;
	source: RevisionSource;
	user: string | null;
	mod: string | null;
	from_version: string | null;
	to_version: string | null;
	replaced_edits: boolean;
	baseline: boolean;
	size_bytes: number;
}

/** The `widget` values the daemon sends, named so a component cannot branch on a string that
 * silently never matches. */
export const widgets = {
	toggle: 'toggle',
	slider: 'slider',
	number: 'number',
	select: 'select',
	multiSelect: 'multi-select',
	text: 'text'
} as const;

export const configs = {
	list: (id: string) => api.get<ConfigList>(`/instances/${id}/configs`),
	read: (id: string, file: string) =>
		api.get<ConfigSchema>(`/instances/${id}/configs/${encodeURIComponent(file)}`),
	/** Keyed by `Section.Key`. Every change is validated before any is written, so a rejected
	 * patch leaves the file exactly as it was (`11 §2.4`). */
	patch: (id: string, file: string, changes: Record<string, ConfigValue>) =>
		api.patch<ConfigSchema>(`/instances/${id}/configs/${encodeURIComponent(file)}`, changes),
	/** The file's kept versions, newest first, without their content. */
	history: (id: string, file: string) =>
		api.get<{ items: ConfigRevision[] }>(
			`/instances/${id}/configs/${encodeURIComponent(file)}/history`
		),
	/** One kept version of a `.cfg`, projected like the file itself. */
	revision: (id: string, file: string, rev: string) =>
		api.get<ConfigCopy>(
			`/instances/${id}/configs/${encodeURIComponent(file)}/history/${encodeURIComponent(rev)}`
		),
	/** The file's own text, with the ETag a save has to hand back (`11 §1.1`). */
	readRaw: (id: string, file: string) =>
		api.getText(`/instances/${id}/configs/${encodeURIComponent(file)}/raw`),
	/** A kept version's own text, gated on `config.raw` like every other route serving bytes. */
	readRawRevision: (id: string, file: string, rev: string) =>
		api.getText(
			`/instances/${id}/configs/${encodeURIComponent(file)}/history/${encodeURIComponent(rev)}/raw`
		),
	/** A full replacement, so a stale `etag` is refused as `stale_write` rather than overwriting
	 * another writer's save. */
	writeRaw: (id: string, file: string, text: string, etag: string) =>
		api.putText(`/instances/${id}/configs/${encodeURIComponent(file)}/raw`, text, etag),
	/** Deletes the file. Its history is kept and shows again if the file comes back. A file an installed mod uses is refused with
	 * `409 mod_conflict` unless `allowInstalled` is set. */
	remove: (id: string, file: string, allowInstalled: boolean) =>
		api.del<void>(
			`/instances/${id}/configs/${encodeURIComponent(file)}?allow_installed=${allowInstalled}`
		)
};

/** The address a `422` names a setting by, and the key a patch body carries. */
export function fieldOf(section: string, key: string): string {
	return section ? `${section}.${key}` : key;
}
