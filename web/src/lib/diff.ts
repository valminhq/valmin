/**
 * A line diff, for showing what a config save changed. Line-based because a `.cfg` edit
 * rewrites exactly the lines whose values changed (B10).
 */

export type DiffKind = 'same' | 'added' | 'removed';

export interface DiffLine {
	kind: DiffKind;
	text: string;
	/** 1-based line number in the old text, or 0 for an added line. */
	before: number;
	/** 1-based line number in the new text, or 0 for a removed line. */
	after: number;
}

/**
 * The most cells the longest-common-subsequence table may hold. Past this the diff degrades to
 * one removed block and one added block rather than allocating a table that could take the tab
 * down with it.
 */
const maxCells = 4_000_000;

function split(text: string): string[] {
	const lines = text.split('\n');
	// A trailing newline ends the last line rather than starting an empty one; without this
	// every file looks like it gained a blank line at the end.
	if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop();
	return lines;
}

export function diffLines(before: string, after: string): DiffLine[] {
	const a = split(before);
	const b = split(after);

	// Trimming the unchanged ends keeps the table below proportional to the change rather than
	// to the file.
	let head = 0;
	while (head < a.length && head < b.length && a[head] === b[head]) head++;
	let tail = 0;
	while (
		tail < a.length - head &&
		tail < b.length - head &&
		a[a.length - 1 - tail] === b[b.length - 1 - tail]
	) {
		tail++;
	}

	const out: DiffLine[] = [];
	for (let i = 0; i < head; i++)
		out.push({ kind: 'same', text: a[i], before: i + 1, after: i + 1 });

	const midA = a.slice(head, a.length - tail);
	const midB = b.slice(head, b.length - tail);
	for (const line of middle(midA, midB, head)) out.push(line);

	for (let i = 0; i < tail; i++) {
		const ai = a.length - tail + i;
		out.push({ kind: 'same', text: a[ai], before: ai + 1, after: b.length - tail + i + 1 });
	}
	return out;
}

/** Diffs the part that actually differs. `offset` is how many lines were trimmed off the
 * front, so the line numbers reported are the file's own. */
function middle(a: string[], b: string[], offset: number): DiffLine[] {
	if (a.length === 0 || b.length === 0 || a.length * b.length > maxCells) {
		return [
			...a.map((text, i) => ({ kind: 'removed' as const, text, before: offset + i + 1, after: 0 })),
			...b.map((text, i) => ({ kind: 'added' as const, text, before: 0, after: offset + i + 1 }))
		];
	}

	// Longest common subsequence, filled from the end so the walk below reads forwards and puts
	// removals ahead of additions at the same position.
	const width = b.length + 1;
	const lcs = new Uint32Array((a.length + 1) * width);
	for (let i = a.length - 1; i >= 0; i--) {
		for (let j = b.length - 1; j >= 0; j--) {
			lcs[i * width + j] =
				a[i] === b[j]
					? lcs[(i + 1) * width + j + 1] + 1
					: Math.max(lcs[(i + 1) * width + j], lcs[i * width + j + 1]);
		}
	}

	const out: DiffLine[] = [];
	let i = 0;
	let j = 0;
	while (i < a.length && j < b.length) {
		if (a[i] === b[j]) {
			out.push({ kind: 'same', text: a[i], before: offset + i + 1, after: offset + j + 1 });
			i++;
			j++;
		} else if (lcs[(i + 1) * width + j] >= lcs[i * width + j + 1]) {
			out.push({ kind: 'removed', text: a[i], before: offset + i + 1, after: 0 });
			i++;
		} else {
			out.push({ kind: 'added', text: b[j], before: 0, after: offset + j + 1 });
			j++;
		}
	}
	for (; i < a.length; i++) {
		out.push({ kind: 'removed', text: a[i], before: offset + i + 1, after: 0 });
	}
	for (; j < b.length; j++) {
		out.push({ kind: 'added', text: b[j], before: 0, after: offset + j + 1 });
	}
	return out;
}

/**
 * Groups a diff into hunks of changed lines with `context` unchanged lines around each. An
 * empty result means the two texts are identical.
 */
export function hunks(lines: DiffLine[], context = 3): DiffLine[][] {
	const keep = lines.map(
		(line, i) =>
			line.kind !== 'same' ||
			lines.slice(Math.max(0, i - context), i + context + 1).some((near) => near.kind !== 'same')
	);

	const out: DiffLine[][] = [];
	let current: DiffLine[] = [];
	for (const [i, line] of lines.entries()) {
		if (keep[i]) {
			current.push(line);
			continue;
		}
		if (current.length > 0) out.push(current);
		current = [];
	}
	if (current.length > 0) out.push(current);
	return out;
}
