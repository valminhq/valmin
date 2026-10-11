import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PlannedShutdown } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import Page from './+page.svelte';

let daemon: FakeDaemon;

const planned: PlannedShutdown = {
	id: 'cut-1',
	power_off_at: '2099-01-01T12:00:00Z',
	created_by_username: null,
	created_by_name: 'Discord: Den (1)'
};

async function open(global: string[], items: PlannedShutdown[] = [planned]) {
	daemon.on('GET', '/admin/shutdowns', () => Response.json({ items, next_cursor: null }));
	daemon.on('POST', '/admin/shutdowns', () => Response.json(planned, { status: 201 }));
	daemon.on('DELETE', '/admin/shutdowns/cut-1', () => new Response(null, { status: 204 }));
	session.permissions = permissions('', [], global);
	render(Page);
	if (global.includes(actions.schedulesGlobal))
		await screen.findByRole('button', { name: 'Add power cut' });
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the power cuts screen', () => {
	it('is closed without schedules.global', async () => {
		await open([actions.view]);
		expect(screen.getByText(/do not have permission/)).toBeTruthy();
		expect(daemon.requests('GET', '/admin/shutdowns')).toHaveLength(0);
	});

	it('lists planned power cuts with who added them', async () => {
		await open([actions.schedulesGlobal]);
		expect(await screen.findByText('Added by Discord: Den (1)')).toBeTruthy();
	});

	it('sends the typed time with the viewer zone', async () => {
		await open([actions.schedulesGlobal], []);
		await fireEvent.input(screen.getByLabelText('The power goes off at'), {
			target: { value: '2099-01-01T14:00' }
		});
		await click(screen.getByRole('button', { name: 'Add power cut' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/shutdowns')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/shutdowns')[0].body).toEqual({
			local_time: '2099-01-01T14:00',
			timezone: expect.any(String)
		});
	});

	it('cancels a power cut once confirmed', async () => {
		vi.stubGlobal('confirm', () => true);
		await open([actions.schedulesGlobal]);
		await click(await screen.findByRole('button', { name: 'Cancel' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/admin/shutdowns/cut-1')).toHaveLength(1)
		);
	});
});
