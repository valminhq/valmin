import { api } from './client';

/** What a setting can hold once the daemon has typed it (`11 §1.1`). */
export type ConfigValue = string | number | boolean;

/** One row of `GET /instances/{id}/configs`. `plugin` comes from the file's own header and is
 * empty when it has none. */
export interface ConfigFile {
	file: string;
	plugin: string;
	size_bytes: number;
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

/**
 * The two versions the panel keeps: `original` is the file as the panel first found it and
 * never moves, `previous` is what the last write replaced. After one save they are identical.
 */
export const copies = ['original', 'previous'] as const;
export type ConfigCopyName = (typeof copies)[number];

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
	/** 404 until the panel has written the file once, meaning there is nothing to compare
	 * against yet. */
	copy: (id: string, file: string, which: ConfigCopyName) =>
		api.get<ConfigCopy>(`/instances/${id}/configs/${encodeURIComponent(file)}/${which}`),
	/** The file's own text, with the ETag a save has to hand back (`11 §1.1`). */
	readRaw: (id: string, file: string) =>
		api.getText(`/instances/${id}/configs/${encodeURIComponent(file)}/raw`),
	/** A kept version's own text, gated on `config.raw` like every other route serving bytes. */
	readRawCopy: (id: string, file: string, which: ConfigCopyName) =>
		api.getText(`/instances/${id}/configs/${encodeURIComponent(file)}/${which}/raw`),
	/** A full replacement, so a stale `etag` is refused as `stale_write` rather than overwriting
	 * another writer's save. */
	writeRaw: (id: string, file: string, text: string, etag: string) =>
		api.putText(`/instances/${id}/configs/${encodeURIComponent(file)}/raw`, text, etag)
};

/** The address a `422` names a setting by, and the key a patch body carries. */
export function fieldOf(section: string, key: string): string {
	return section ? `${section}.${key}` : key;
}
