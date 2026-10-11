import { describe, expect, it } from 'vitest';
import { findMatches, splitMatches, type SearchPart } from './console-search';

const lines = [
	'[Info] Server started',
	'[Warning] Low disk',
	'[ERROR] Failed to bind port',
	'',
	'error: again, ERROR twice',
	'a.b*c (literal) [x]'
];

describe('findMatches', () => {
	const cases: { name: string; query: string; want: number[] }[] = [
		{ name: 'an empty query matches nothing', query: '', want: [] },
		{ name: 'ignores case in both directions', query: 'error', want: [2, 4] },
		{ name: 'matches a substring, not a word', query: 'ail', want: [2] },
		{ name: 'a query with no hit finds nothing', query: 'zzz', want: [] },
		{ name: 'keeps every hit in order', query: 'e', want: [0, 2, 4, 5] },
		{ name: 'treats regular expression characters literally', query: 'a.b*c', want: [5] },
		{ name: 'a dot is not a wildcard', query: 'a.c', want: [] },
		{ name: 'brackets and parentheses are literal', query: '(literal) [x]', want: [5] },
		{ name: 'whitespace is part of the query', query: ' low ', want: [1] }
	];

	for (const { name, query, want } of cases) {
		it(name, () => expect(findMatches(lines, query)).toEqual(want));
	}

	it('finds nothing in no lines', () => {
		expect(findMatches([], 'x')).toEqual([]);
	});
});

describe('splitMatches', () => {
	const plain = (text: string): SearchPart => ({ text, match: false });
	const hit = (text: string): SearchPart => ({ text, match: true });

	const cases: { name: string; text: string; query: string; want: SearchPart[] }[] = [
		{ name: 'an empty query leaves the line whole', text: 'abc', query: '', want: [plain('abc')] },
		{ name: 'an empty line has no parts', text: '', query: 'a', want: [] },
		{ name: 'no hit leaves the line whole', text: 'abc', query: 'z', want: [plain('abc')] },
		{
			name: 'marks one hit between two plain stretches',
			text: 'xxERRORyy',
			query: 'error',
			want: [plain('xx'), hit('ERROR'), plain('yy')]
		},
		{ name: 'a hit that is the whole line', text: 'Error', query: 'error', want: [hit('Error')] },
		{
			name: 'a hit at either end has no empty neighbour',
			text: 'ab..ab',
			query: 'ab',
			want: [hit('ab'), plain('..'), hit('ab')]
		},
		{
			name: 'adjacent hits stay separate',
			text: 'aaaa',
			query: 'aa',
			want: [hit('aa'), hit('aa')]
		},
		{
			name: 'keeps the original casing of each hit',
			text: 'Err err ERR',
			query: 'err',
			want: [hit('Err'), plain(' '), hit('err'), plain(' '), hit('ERR')]
		},
		{
			name: 'regular expression characters are literal',
			text: 'a.b axb',
			query: 'a.b',
			want: [hit('a.b'), plain(' axb')]
		}
	];

	for (const { name, text, query, want } of cases) {
		it(name, () => {
			const parts = splitMatches(text, query);
			expect(parts).toEqual(want);
			expect(parts.map((part) => part.text).join('')).toBe(text);
		});
	}
});
