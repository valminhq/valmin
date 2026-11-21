import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import SaveFooter from './save-footer.svelte';

describe('the card save row', () => {
	it('keeps Save disabled, and says nothing, until something differs', () => {
		render(SaveFooter, {
			saveLabel: 'Save x',
			changed: false,
			ondiscard: vi.fn(),
			onsave: vi.fn()
		});

		expect((screen.getByRole('button', { name: 'Save x' }) as HTMLButtonElement).disabled).toBe(
			true
		);
		expect(screen.queryByText('Unsaved changes')).toBeNull();
		expect(screen.queryByRole('button', { name: 'Discard changes' })).toBeNull();
	});

	it('offers Save, the note and Discard once something differs', async () => {
		const onsave = vi.fn();
		const ondiscard = vi.fn();
		render(SaveFooter, { saveLabel: 'Save x', changed: true, ondiscard, onsave });

		await fireEvent.click(screen.getByRole('button', { name: 'Save x' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Discard changes' }));
		expect(screen.getByText('Unsaved changes')).toBeTruthy();
		expect(onsave).toHaveBeenCalledOnce();
		expect(ondiscard).toHaveBeenCalledOnce();
	});

	// A form that creates what it describes saves with nothing changed, and must not claim
	// unsaved work it does not have.
	it('enables Save for a form that creates, without the unsaved note', () => {
		render(SaveFooter, {
			saveLabel: 'Save x',
			changed: false,
			creating: true,
			ondiscard: vi.fn(),
			onsave: vi.fn()
		});

		expect((screen.getByRole('button', { name: 'Save x' }) as HTMLButtonElement).disabled).toBe(
			false
		);
		expect(screen.queryByText('Unsaved changes')).toBeNull();
	});

	// Two forms on one page share a busy flag to stay disabled, but only the one saving says so.
	it('reads Saving… only for the form that is saving', () => {
		render(SaveFooter, {
			saveLabel: 'Save x',
			changed: true,
			busy: true,
			saving: false,
			ondiscard: vi.fn(),
			onsave: vi.fn()
		});

		expect(screen.getByRole('button', { name: 'Save x' })).toBeTruthy();
		expect(screen.queryByText('Saving…')).toBeNull();
	});
});
