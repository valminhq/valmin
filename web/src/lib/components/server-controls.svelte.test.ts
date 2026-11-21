import { render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import { FakeDaemon, instance } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import ServerControls from './server-controls.svelte';

let daemon: FakeDaemon;
const all = [actions.start, actions.stop, actions.restart];
const accepted = () => Response.json({ job_id: 'job-1' }, { status: 202 });

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => vi.unstubAllGlobals());

function show(state: string, allowed: string[] = all) {
	const onfailure = vi.fn();
	render(ServerControls, {
		props: { instance: instance({ id: 'inst-a', name: 'Live', state }), allowed, onfailure }
	});
	return onfailure;
}

describe('the server controls', () => {
	it('renders nothing for a viewer', () => {
		show('running', []);
		expect(screen.queryByRole('group', { name: 'Controls for Live' })).toBeNull();
	});

	it('offers only Start on a stopped server, and starts without asking', async () => {
		daemon.on('POST', '/instances/inst-a/start', accepted);
		show('stopped');
		expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();

		await click(screen.getByRole('button', { name: 'Start' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/start')).toHaveLength(1)
		);
	});

	it('asks before stopping a running server', async () => {
		daemon.on('POST', '/instances/inst-a/stop', accepted);
		show('running');
		expect(screen.queryByRole('button', { name: 'Start' })).toBeNull();

		await click(screen.getByRole('button', { name: 'Stop' }));
		expect(daemon.requests('POST', '/instances/inst-a/stop')).toHaveLength(0);

		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Stop Live?');
		await click(within(dialog).getByRole('button', { name: 'Stop' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/stop')).toHaveLength(1)
		);
	});

	it('reports a refused request to its owner', async () => {
		daemon.on('POST', '/instances/inst-a/start', () =>
			Response.json({ error: { code: 'conflict', message: 'busy' } }, { status: 409 })
		);
		const onfailure = show('stopped');
		await click(screen.getByRole('button', { name: 'Start' }));
		await vi.waitFor(() => expect(onfailure).toHaveBeenCalledTimes(2));
		expect(onfailure.mock.calls[0][0]).toBeNull();
		expect(onfailure.mock.calls[1][0]).toBeTruthy();
	});
});
