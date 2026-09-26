import { render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { click } from '$lib/testing/interact';
import CopyButton from './copy-button.svelte';

function clipboard(writeText: (text: string) => Promise<void>) {
	Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
}

afterEach(() => {
	Reflect.deleteProperty(navigator, 'clipboard');
});

// The clipboard API is refused over plain HTTP, which is how a panel on a LAN address is
// reached. A copy that fails silently is a value the operator believes they have.
describe('copying a value', () => {
	it('copies the value, says so, and goes back to its label', async () => {
		// The label resets on a timer; on the fake clock it cannot outlive the test.
		vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
		const writeText = vi.fn(() => Promise.resolve());
		clipboard(writeText);
		render(CopyButton, { value: '482913', label: 'Copy code' });

		await click(screen.getByRole('button', { name: 'Copy code' }));
		expect(writeText).toHaveBeenCalledWith('482913');
		await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Copied' })).toBeTruthy());

		await vi.advanceTimersByTimeAsync(2000);
		expect(screen.getByRole('button', { name: 'Copy code' })).toBeTruthy();
		vi.useRealTimers();
	});

	it('says the copy was refused, and what to do instead', async () => {
		clipboard(() => Promise.reject(new DOMException('Not allowed', 'NotAllowedError')));
		render(CopyButton, { value: '482913', label: 'Copy code' });

		await click(screen.getByRole('button', { name: 'Copy code' }));
		expect(await screen.findByText(/Select the text and copy it by hand\./)).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Copied' })).toBeNull();
	});
});
