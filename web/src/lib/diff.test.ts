import { describe, expect, it } from 'vitest';
import { diffLines, hunks, type DiffLine } from './diff';

/** The diff as a git-shaped string, which is what the assertions are actually about. */
function render(before: string, after: string): string {
	return diffLines(before, after)
		.map((line) => ({ same: ' ', added: '+', removed: '-' })[line.kind] + line.text)
		.join('\n');
}

describe('diffLines', () => {
	it('reports nothing for identical text', () => {
		const lines = diffLines('a\nb\nc\n', 'a\nb\nc\n');
		expect(lines.every((line) => line.kind === 'same')).toBe(true);
		expect(hunks(lines)).toEqual([]);
	});

	it('shows one changed line as a removal and an addition', () => {
		expect(render('a\nb\nc\n', 'a\nB\nc\n')).toBe(' a\n-b\n+B\n c');
	});

	// The case the trim exists for, and the one a config save actually produces: two edits
	// with untouched text between and around them.
	it('keeps scattered changes apart instead of merging them into one block', () => {
		const before = 'one\ntwo\nthree\nfour\nfive\nsix\n';
		const after = 'one\nTWO\nthree\nfour\nFIVE\nsix\n';
		expect(render(before, after)).toBe(' one\n-two\n+TWO\n three\n four\n-five\n+FIVE\n six');
	});

	it('handles a pure insertion and a pure deletion', () => {
		expect(render('a\nc\n', 'a\nb\nc\n')).toBe(' a\n+b\n c');
		expect(render('a\nb\nc\n', 'a\nc\n')).toBe(' a\n-b\n c');
	});

	it('handles an empty side', () => {
		expect(render('', 'a\n')).toBe('-\n+a');
		expect(diffLines('a\nb\n', '')).toEqual([
			{ kind: 'removed', text: 'a', before: 1, after: 0 },
			{ kind: 'removed', text: 'b', before: 2, after: 0 },
			{ kind: 'added', text: '', before: 0, after: 1 }
		]);
	});

	// A trailing newline ends the last line rather than starting an empty one. Without this
	// every config in the corpus reads as having gained a blank line.
	it('does not invent a trailing blank line', () => {
		expect(render('a\n', 'a')).toBe(' a');
	});

	it('numbers lines against each side', () => {
		const lines = diffLines('a\nb\n', 'a\nB\n');
		expect(lines.map((l) => [l.kind, l.before, l.after])).toEqual([
			['same', 1, 1],
			['removed', 2, 0],
			['added', 0, 2]
		]);
	});
});

describe('hunks', () => {
	const many = (n: number, prefix = 'line') =>
		Array.from({ length: n }, (_, i) => `${prefix}${i}`).join('\n');

	it('shows only the changed neighbourhood of a long file', () => {
		const before = many(60);
		const after = before.replace('line30', 'CHANGED');
		const grouped = hunks(diffLines(before, after), 3);

		expect(grouped).toHaveLength(1);
		expect(grouped[0].map((l) => l.text)).toEqual([
			'line27',
			'line28',
			'line29',
			'line30',
			'CHANGED',
			'line31',
			'line32',
			'line33'
		]);
	});

	it('splits changes that are far apart into separate hunks', () => {
		const before = many(60);
		const after = before.replace('line10', 'A').replace('line50', 'B');
		const grouped = hunks(diffLines(before, after), 3);

		expect(grouped).toHaveLength(2);
		expect(grouped[0].some((l: DiffLine) => l.text === 'A')).toBe(true);
		expect(grouped[1].some((l: DiffLine) => l.text === 'B')).toBe(true);
	});
});
