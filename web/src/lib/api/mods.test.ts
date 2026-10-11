import { describe, expect, it } from 'vitest';
import { sharedPrefix } from '$lib/api/mods';

describe('sharedPrefix', () => {
	const cases: Array<[name: string, names: string[], want: string]> = [
		['nothing to share', [], ''],
		['one name is its own prefix', ['Author.Sailing.cfg'], 'Author.Sailing.cfg'],
		[
			'keeps the text every name starts with',
			['Author.Biomes.Ashlands.cfg', 'Author.Biomes.cfg'],
			'Author.Biomes.'
		],
		['a name that is a prefix of another', ['a.cfg', 'a.cfg.old'], 'a.cfg'],
		['names that share nothing', ['Author.Sailing.cfg', 'com.example.wards.cfg'], ''],
		['compares case exactly', ['Author.cfg', 'author.cfg'], ''],
		['never ends inside a character', ['a\u{1F600}.cfg', 'a\u{1F601}.cfg'], 'a'],
		['looks at every name, not only the first two', ['ab.cfg', 'ab.x.cfg', 'ac.cfg'], 'a']
	];

	it.each(cases)('%s', (_name, names, want) => {
		expect(sharedPrefix(names)).toBe(want);
	});
});
