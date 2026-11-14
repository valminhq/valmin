import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, instance, permissions } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import WatchdogPanel from './watchdog-panel.svelte';

let daemon: FakeDaemon;

function open(held: string[], watchdog: 'off' | 'alert' | 'restart' = 'off') {
	session.permissions = permissions('inst-a', held);
	render(WatchdogPanel, { instance: instance({ watchdog }) });
}

function save(): HTMLButtonElement {
	return screen.getByRole('button', { name: 'Save watchdog' }) as HTMLButtonElement;
}

function choice(): HTMLSelectElement {
	return screen.getByLabelText('When the server stops responding') as HTMLSelectElement;
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

describe('the watchdog panel', () => {
	it('saves an alert', async () => {
		open([actions.settings]);
		expect(save().disabled).toBe(true);
		await fireEvent.change(choice(), { target: { value: 'alert' } });
		await vi.waitFor(() => expect(save().disabled).toBe(false));
		await click(save());

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({ watchdog: 'alert' });
	});

	it('saves an automatic restart for someone who may restart the server', async () => {
		open([actions.settings, actions.restart], 'alert');
		await fireEvent.change(choice(), { target: { value: 'restart' } });
		await click(save());

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({ watchdog: 'restart' });
	});

	it('offers the restart only to someone who may restart the server', () => {
		open([actions.settings]);
		const option = screen.getByRole('option', {
			name: 'Alert me and restart the server'
		}) as HTMLOptionElement;
		expect(option.disabled).toBe(true);
	});

	it('says what counts as not responding', () => {
		open([actions.settings]);
		expect(screen.getByText(/no status line for 15 minutes/)).toBeTruthy();
	});

	it.each([[[actions.view]], [[actions.restart]]])('is read-only with only %j', (held) => {
		open(held, 'alert');
		expect(screen.queryByRole('button', { name: 'Save watchdog' })).toBeNull();
		expect(choice().disabled).toBe(true);
	});
});
