import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import ConnectionSummary from './connection-summary.svelte';

let daemon: FakeDaemon;

const reads = (id = 'inst-a') => daemon.requests('GET', `/instances/${id}/password`);

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/instances/inst-a/password', () => Response.json({ password: 'hunter22' }));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the password row', () => {
	it('requests nothing until Show is pressed', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance() });

		expect(screen.getByRole('button', { name: 'Show' })).toBeTruthy();
		expect(screen.queryByText('hunter22')).toBeNull();
		expect(reads()).toHaveLength(0);
	});

	it('reads the password once on Show, offers a copy, and forgets it on Hide', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance() });

		await click(screen.getByRole('button', { name: 'Show' }));
		const value = await screen.findByText('hunter22');
		expect(value.tagName).toBe('CODE');
		expect(screen.getByRole('button', { name: 'Copy password' })).toBeTruthy();
		expect(reads()).toHaveLength(1);

		await click(screen.getByRole('button', { name: 'Hide' }));
		expect(screen.queryByText('hunter22')).toBeNull();
		expect(screen.getByRole('button', { name: 'Show' })).toBeTruthy();
		expect(reads()).toHaveLength(1);
	});

	it('shows the daemon error inline when the read fails', async () => {
		daemon.on('GET', '/instances/inst-a/password', () =>
			envelope(500, 'internal', 'Something went wrong.')
		);
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance() });

		await click(screen.getByRole('button', { name: 'Show' }));
		expect(await screen.findByText('Something went wrong.')).toBeTruthy();
	});

	it('forgets a revealed password when the instance changes', async () => {
		session.permissions = {
			...permissions('inst-a', [actions.view]),
			instances: [
				{ instance_id: 'inst-a', allowed_actions: [actions.view] },
				{ instance_id: 'inst-b', allowed_actions: [actions.view] }
			]
		};
		const view = render(ConnectionSummary, { instance: instance() });
		await click(screen.getByRole('button', { name: 'Show' }));
		await screen.findByText('hunter22');

		await view.rerender({ instance: instance({ id: 'inst-b' }) });
		expect(screen.queryByText('hunter22')).toBeNull();
		expect(reads('inst-b')).toHaveLength(0);
	});

	it('keeps a revealed password when the same instance is updated', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance() });
		await click(screen.getByRole('button', { name: 'Show' }));
		await screen.findByText('hunter22');

		await view.rerender({ instance: instance({ state: 'running', restart_required: true }) });
		expect(screen.getByText('hunter22')).toBeTruthy();
		expect(reads()).toHaveLength(1);
	});

	it('drops the error of a read that settles after the instance changed', async () => {
		let fail!: () => void;
		daemon.on(
			'GET',
			'/instances/inst-a/password',
			() =>
				new Promise<Response>((resolve) => {
					fail = () => resolve(envelope(500, 'internal', 'Something went wrong.'));
				})
		);
		session.permissions = {
			...permissions('inst-a', [actions.view]),
			instances: [
				{ instance_id: 'inst-a', allowed_actions: [actions.view] },
				{ instance_id: 'inst-b', allowed_actions: [actions.view] }
			]
		};
		const view = render(ConnectionSummary, { instance: instance() });
		await click(screen.getByRole('button', { name: 'Show' }));
		await vi.waitFor(() => expect(reads()).toHaveLength(1));

		await view.rerender({ instance: instance({ id: 'inst-b' }) });
		fail();
		await new Promise((r) => setTimeout(r, 0));
		expect(screen.queryByText('Something went wrong.')).toBeNull();
	});

	it('notes a pending restart only when one is required', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance({ restart_required: true }) });
		expect(screen.getByText(/keeps the previous password until it restarts/)).toBeTruthy();

		view.unmount();
		render(ConnectionSummary, { instance: instance() });
		expect(screen.queryByText(/keeps the previous password/)).toBeNull();
	});

	it('is absent without instance.view', () => {
		session.permissions = permissions('inst-a', []);
		render(ConnectionSummary, { instance: instance({ restart_required: true }) });

		expect(screen.queryByText('Password')).toBeNull();
		expect(screen.queryByRole('button', { name: 'Show' })).toBeNull();
		expect(reads()).toHaveLength(0);
	});
});
