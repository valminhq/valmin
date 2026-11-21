import { describe, expect, it } from 'vitest';
import type { ConfigFile, ConfigRevision } from '#lib/api/configs.js';
import { filterConfigs, newerModVersion, revisionLabel } from '#lib/config-files.js';

const files: ConfigFile[] = [
	{
		file: 'Author.Sailing.cfg',
		plugin: 'Sailing Overhaul',
		size_bytes: 120,
		installed_mods: [],
		dir: 'BepInEx/config',
		portable: true,
		mod_replaced_edits: null
	},
	{
		file: 'com.example.wards.cfg',
		plugin: 'Wards',
		size_bytes: 80,
		installed_mods: [],
		dir: 'BepInEx/config',
		portable: true,
		mod_replaced_edits: null
	},
	{
		file: 'orphan.cfg',
		plugin: '',
		size_bytes: 10,
		installed_mods: [],
		dir: 'BepInEx/config',
		portable: true,
		mod_replaced_edits: null
	}
];

const all = files.map((f) => f.file);

describe('filterConfigs', () => {
	const cases: Array<[name: string, query: string, want: string[]]> = [
		['a blank query keeps every file', '', all],
		['whitespace alone is blank', '   ', all],
		['matches part of a filename', 'sailing.c', ['Author.Sailing.cfg']],
		['matches part of a plugin name', 'overhaul', ['Author.Sailing.cfg']],
		['ignores case in the query', 'WARDS', ['com.example.wards.cfg']],
		['ignores case in the file', 'author', ['Author.Sailing.cfg']],
		['trims the query', '  orphan  ', ['orphan.cfg']],
		['lists a file once when both fields match', 'ward', ['com.example.wards.cfg']],
		['returns nothing when nothing matches', 'zzz', []]
	];

	it.each(cases)('%s', (_name, query, want) => {
		expect(filterConfigs(files, query).map((f) => f.file)).toEqual(want);
	});

	it('keeps the order it was given', () => {
		const reversed = [...files].reverse();
		expect(filterConfigs(reversed, '.cfg').map((f) => f.file)).toEqual(all.toReversed());
	});
});

function rev(id: string, over: Partial<ConfigRevision>): ConfigRevision {
	return {
		id,
		created_at: '2026-11-01T10:00:00Z',
		source: 'edit',
		user: null,
		mod: null,
		from_version: null,
		to_version: null,
		replaced_edits: false,
		baseline: false,
		size_bytes: 10,
		...over
	};
}

describe('revisionLabel', () => {
	it('names what produced each version', () => {
		expect(revisionLabel(rev('a', { source: 'found' }))).toBe('As first found');
		expect(revisionLabel(rev('a', { source: 'edit', user: 'den' }))).toBe('Saved by den');
		expect(
			revisionLabel(
				rev('a', { source: 'mod', mod: 'Ns-Mod', from_version: '1.0', to_version: '2.0' })
			)
		).toBe('Ns-Mod 1.0 → 2.0');
		expect(revisionLabel(rev('a', { source: 'mod', mod: 'Ns-Mod', to_version: '1.0' }))).toBe(
			'Installed with Ns-Mod 1.0'
		);
	});
});

describe('newerModVersion', () => {
	const history = [
		rev('edit-after', {}),
		rev('mod', { source: 'mod', mod: 'Ns-Mod', to_version: '2.0' }),
		rev('edit-before', {})
	];
	it('names the mod version a chosen version predates', () => {
		expect(newerModVersion(history, 'edit-before')?.id).toBe('mod');
	});
	it('says nothing for a version at or after the newest mod version', () => {
		expect(newerModVersion(history, 'edit-after')).toBeNull();
		expect(newerModVersion(history, 'mod')).toBeNull();
		expect(newerModVersion(history, 'missing')).toBeNull();
	});
});
