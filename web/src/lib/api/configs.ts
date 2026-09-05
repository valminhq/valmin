import { api } from './client';

/** What a setting can hold once the daemon has typed it (`11 §1.1`). */
export type ConfigValue = string | number | boolean;

/** One row of `GET /instances/{id}/configs`. `plugin` comes from the file's own header and
 * is empty when it has none — the file name is then all there is to show. */
export interface ConfigFile {
	file: string;
	plugin: string;
	size_bytes: number;
}

export interface ConfigList {
	items: ConfigFile[];
	/** The daemon's sentence about why the list looks the way it does, rendered as sent.
	 * Composing it here would need Valheim knowledge the SPA does not hold (F2, ADR-110). */
	note?: string;
}

export interface ConfigRange {
	min: number;
	max: number;
}

/**
 * One setting, as `04 §3` serves it.
 *
 * `widget` is the control to render and the daemon decides it — the SPA never sees a type
 * name it would have to interpret (F2, `02 §2.1`). `type` is carried for the operator's
 * benefit and is deliberately never branched on here. `step` sizes a numeric control for
 * the same reason `widget` exists: only the daemon knows which types hold whole numbers.
 * Zero means continuous.
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
 * The file as the panel first found it, from the copy taken before its first write.
 *
 * Not the previous save: that copy moves with every write, this one never does. `captured_at`
 * is served because it is the only thing that says how old this is — a plugin that rewrites
 * its config makes the copy arbitrarily stale without changing it.
 */
export interface ConfigOriginal extends ConfigSchema {
	captured_at: string;
}

/** The `widget` values the daemon sends. Named here so a component cannot branch on a
 * string that silently never matches. */
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
	/** Keyed by `Section.Key`. Every change is validated before any is written, so a
	 * rejected patch leaves the file exactly as it was (`11 §2.4`). */
	patch: (id: string, file: string, changes: Record<string, ConfigValue>) =>
		api.patch<ConfigSchema>(`/instances/${id}/configs/${encodeURIComponent(file)}`, changes),
	/** 404 until the panel has written the file once, which is not an error — there is
	 * simply nothing to compare against yet. */
	original: (id: string, file: string) =>
		api.get<ConfigOriginal>(`/instances/${id}/configs/${encodeURIComponent(file)}/original`),
	/** The file's own text, with the ETag a save has to hand back (`11 §1.1`). */
	readRaw: (id: string, file: string) =>
		api.getText(`/instances/${id}/configs/${encodeURIComponent(file)}/raw`),
	/** A full replacement, so a stale `etag` is refused as `stale_write` rather than
	 * overwriting whatever the other writer saved. */
	writeRaw: (id: string, file: string, text: string, etag: string) =>
		api.putText(`/instances/${id}/configs/${encodeURIComponent(file)}/raw`, text, etag)
};

/** The address a `422` names a setting by, and the key a patch body carries. */
export function fieldOf(section: string, key: string): string {
	return section ? `${section}.${key}` : key;
}
