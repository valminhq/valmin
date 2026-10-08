import { describe, expect, it } from 'vitest';
import type { ConfigFile } from '$lib/api/configs';
import { filterConfigs } from '$lib/config-files';

const files: ConfigFile[] = [
	{ file: 'Author.Sailing.cfg', plugin: 'Sailing Overhaul', size_bytes: 120, installed_mods: [] },
	{ file: 'com.example.wards.cfg', plugin: 'Wards', size_bytes: 80, installed_mods: [] },
	{ file: 'orphan.cfg', plugin: '', size_bytes: 10, installed_mods: [] }
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
