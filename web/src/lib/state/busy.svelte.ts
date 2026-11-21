import { untrack } from 'svelte';

/** How long after a navigation a read counts as loading the new page. */
const PAGE_LOAD_WINDOW_MS = 5000;

/** The reads still in flight that are loading a page, for the progress bar. */
export const loading = $state({ count: 0 });

let navigatedAt = Date.now();

/** Marks the moment a navigation starts, so the reads that follow count as loading its page. */
export function navigationStarted(): void {
	navigatedAt = Date.now();
}

/** Runs a read, counting it as page loading when it starts soon after a navigation. Reads
 * started later, such as polls, run uncounted. The count is changed untracked, because reads
 * start inside effects that must not depend on it, and the caller gets the read's own promise
 * so counting adds no delay. */
export function trackPageRead<T>(read: () => Promise<T>): Promise<T> {
	const started = read();
	if (Date.now() - navigatedAt > PAGE_LOAD_WINDOW_MS) return started;
	untrack(() => loading.count++);
	const settled = () => untrack(() => loading.count--);
	started.then(settled, settled);
	return started;
}
