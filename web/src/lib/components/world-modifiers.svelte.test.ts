import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { click } from '#lib/testing/interact.js';
import { gameOptions } from '#lib/testing/daemon.js';
import WorldModifiers from './world-modifiers.svelte';

function show(initial: Record<string, string> = {}) {
	const modifiers = $state(initial);
	render(WorldModifiers, { options: gameOptions(), modifiers });
	return modifiers;
}

async function slide(label: string, position: number) {
	const range = screen.getByLabelText(label) as HTMLInputElement;
	range.value = String(position);
	await fireEvent.input(range);
}

describe('world modifiers', () => {
	it('sets an axis from its slider and drops it at Normal', async () => {
		const modifiers = show();
		expect(screen.getByText('Normal')).toBeTruthy();

		await slide('Combat', 2);
		expect(modifiers).toEqual({ combat: 'hard' });
		expect(screen.getByText('Hard')).toBeTruthy();

		await slide('Combat', 1);
		expect(modifiers).toEqual({});
	});

	it('sets a world key to on and removes it when switched off', async () => {
		const modifiers = show({ other: 'kept' });

		await click(screen.getByRole('switch', { name: 'No map' }));
		expect(modifiers).toEqual({ other: 'kept', nomap: 'on' });

		await click(screen.getByRole('switch', { name: 'No map' }));
		expect(modifiers).toEqual({ other: 'kept' });
	});

	it('shows a value no slider position holds as text and keeps manual mode on', () => {
		show({ combat: 'brutal' });

		expect((screen.getByLabelText('Combat') as HTMLInputElement).value).toBe('brutal');
		const manual = screen.getByRole('switch', { name: 'Manual' });
		expect(manual.getAttribute('aria-checked')).toBe('true');
		expect(manual.hasAttribute('disabled')).toBe(true);
	});

	it('types values by hand in manual mode', async () => {
		const modifiers = show();

		await click(screen.getByRole('switch', { name: 'Manual' }));
		await fireEvent.input(screen.getByLabelText('Combat'), { target: { value: 'veryhard' } });
		expect(modifiers).toEqual({ combat: 'veryhard' });
	});
});
