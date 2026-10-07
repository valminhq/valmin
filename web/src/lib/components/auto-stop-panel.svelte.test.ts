import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import AutoStopPanel from './auto-stop-panel.svelte';

let daemon: FakeDaemon;

function open(held: string[], minutes = 0) {
	session.permissions = permissions('inst-a', held);
	render(AutoStopPanel, { instance: instance({ auto_stop_minutes: minutes }) });
}

function save(): HTMLButtonElement {
	return screen.getByRole('button', { name: 'Save auto-stop' }) as HTMLButtonElement;
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('PATCH', '/instances/inst-a', () => Response.json(instance()));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the auto-stop panel', () => {
	it('turns auto-stop on with the chosen delay', async () => {
		open([actions.settings, actions.stop]);
		await click(screen.getByRole('switch', { name: 'Stop when empty' }));
		await fireEvent.input(screen.getByLabelText('Minutes with no players'), {
			target: { value: '45' }
		});
		await vi.waitFor(() => expect(save().disabled).toBe(false));
		await click(save());

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({
			auto_stop_minutes: 45
		});
	});

	it('turns auto-stop off by saving zero', async () => {
		open([actions.settings, actions.stop], 30);
		await click(screen.getByRole('switch', { name: 'Stop when empty' }));
		await click(save());

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({
			auto_stop_minutes: 0
		});
	});

	it('will not save a delay below the minimum', async () => {
		open([actions.settings, actions.stop], 30);
		await fireEvent.input(screen.getByLabelText('Minutes with no players'), {
			target: { value: '2' }
		});
		await vi.waitFor(() => expect(save().disabled).toBe(true));
		expect(screen.getByText(/Enter a whole number from 5 to 1440/)).toBeTruthy();
	});

	it.each([[[actions.view]], [[actions.settings]], [[actions.stop]]])(
		'is read-only with only %j',
		(held) => {
			open(held, 30);
			expect(screen.queryByRole('button', { name: 'Save auto-stop' })).toBeNull();
			expect(
				(screen.getByRole('switch', { name: 'Stop when empty' }) as HTMLButtonElement).disabled
			).toBe(true);
		}
	);
});
