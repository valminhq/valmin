/** A piece of a line: either plain text or a stretch that matched the search. */
export type SearchPart = { text: string; match: boolean };

/** A case-insensitive literal pattern for `query`, or null when there is nothing to look for. */
function literal(query: string, flags: string): RegExp | null {
	if (query === '') return null;
	return new RegExp(query.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), `i${flags}`);
}

/** The indexes of the lines that contain `query`, ignoring case. An empty query matches nothing. */
export function findMatches(lines: readonly string[], query: string): number[] {
	const pattern = literal(query, '');
	if (!pattern) return [];
	const found: number[] = [];
	lines.forEach((line, index) => {
		if (pattern.test(line)) found.push(index);
	});
	return found;
}

/**
 * Splits `text` into consecutive parts, marking each stretch that equals `query` ignoring
 * case. Joining the parts gives `text` back unchanged.
 */
export function splitMatches(text: string, query: string): SearchPart[] {
	if (text === '') return [];
	const pattern = literal(query, 'g');
	if (!pattern) return [{ text, match: false }];
	const parts: SearchPart[] = [];
	let end = 0;
	for (const found of text.matchAll(pattern)) {
		if (found.index > end) parts.push({ text: text.slice(end, found.index), match: false });
		parts.push({ text: found[0], match: true });
		end = found.index + found[0].length;
	}
	if (end < text.length) parts.push({ text: text.slice(end), match: false });
	return parts;
}
