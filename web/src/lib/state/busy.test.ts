import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { loading, navigationStarted, trackPageRead } from './busy.svelte';

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('page-load tracking', () => {
	// The bar the root layout draws counts these, so a read that fails must still be released.
	it('counts a read shortly after a navigation until it settles', async () => {
		navigationStarted();
		let finish: () => void = () => {};
		const read = trackPageRead(() => new Promise<void>((resolve) => (finish = resolve)));
		expect(loading.count).toBe(1);
		finish();
		await read;
		expect(loading.count).toBe(0);
	});

	it('releases the count when the read fails', async () => {
		navigationStarted();
		await expect(trackPageRead(() => Promise.reject(new Error('down')))).rejects.toThrow('down');
		expect(loading.count).toBe(0);
	});

	it('leaves a read started long after a navigation uncounted', async () => {
		navigationStarted();
		vi.advanceTimersByTime(6000);
		const read = trackPageRead(() => Promise.resolve());
		expect(loading.count).toBe(0);
		await read;
	});
});
