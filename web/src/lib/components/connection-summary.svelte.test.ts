import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, envelope, instance, permissions } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
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

describe('the reachability check', () => {
	const checks = (id = 'inst-a') => daemon.requests('GET', `/instances/${id}/reachability`);
	const report = (over: Record<string, unknown> = {}) => ({
		status: 'warn',
		public_address: '203.0.113.7:2456',
		detail: 'The server did not answer at 203.0.113.7:2456 when asked from the panel host.',
		remedy: 'Forward UDP ports 2456-2457 on the router to this host.',
		steps: [
			{ name: 'public_ip', status: 'ok', detail: "The host's public address is 203.0.113.7." },
			{ name: 'server_query', status: 'warn', detail: 'No answer from 203.0.113.7:2457 (UDP).' }
		],
		checked_at: '2026-10-25T10:00:00Z',
		...over
	});

	beforeEach(() => {
		daemon.on('GET', '/instances/inst-a/reachability', () => Response.json(report()));
	});

	it('asks nothing until the button is pressed', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		expect(screen.getByRole('button', { name: 'Check reachability' })).toBeTruthy();
		expect(checks()).toHaveLength(0);
	});

	it('shows the verdict, the address to copy, the remedy and each step', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		await click(screen.getByRole('button', { name: 'Check reachability' }));
		expect(await screen.findByText(/did not answer at 203\.0\.113\.7:2456/)).toBeTruthy();
		expect(screen.getByText('203.0.113.7:2456').tagName).toBe('CODE');
		expect(screen.getByRole('button', { name: 'Copy join address' })).toBeTruthy();
		expect(screen.getByText(/Forward UDP ports 2456-2457/)).toBeTruthy();
		expect(screen.getByText('No answer from 203.0.113.7:2457 (UDP).')).toBeTruthy();
		expect(checks()).toHaveLength(1);
	});

	it('offers no address when the verdict names none', async () => {
		daemon.on('GET', '/instances/inst-a/reachability', () =>
			Response.json(
				report({ status: 'unknown', public_address: undefined, remedy: undefined, steps: [] })
			)
		);
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		await click(screen.getByRole('button', { name: 'Check reachability' }));
		await screen.findByRole('status');
		expect(screen.queryByRole('button', { name: 'Copy join address' })).toBeNull();
	});

	it('shows the daemon error inline when the check fails', async () => {
		daemon.on('GET', '/instances/inst-a/reachability', () =>
			envelope(429, 'rate_limited', 'Too many requests. Try again shortly.')
		);
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		await click(screen.getByRole('button', { name: 'Check reachability' }));
		expect(await screen.findByText('Too many requests. Try again shortly.')).toBeTruthy();
	});

	it('forgets a result when the instance changes', async () => {
		session.permissions = {
			...permissions('inst-a', [actions.view]),
			instances: [
				{ instance_id: 'inst-a', allowed_actions: [actions.view] },
				{ instance_id: 'inst-b', allowed_actions: [actions.view] }
			]
		};
		const view = render(ConnectionSummary, { instance: instance({ state: 'running' }) });
		await click(screen.getByRole('button', { name: 'Check reachability' }));
		await screen.findByRole('status');

		await view.rerender({ instance: instance({ id: 'inst-b', state: 'running' }) });
		expect(screen.queryByRole('status')).toBeNull();
	});

	it('drops a result that arrives after the instance changed', async () => {
		let answer!: () => void;
		daemon.on(
			'GET',
			'/instances/inst-a/reachability',
			() =>
				new Promise<Response>((resolve) => {
					answer = () => resolve(Response.json(report()));
				})
		);
		session.permissions = {
			...permissions('inst-a', [actions.view]),
			instances: [
				{ instance_id: 'inst-a', allowed_actions: [actions.view] },
				{ instance_id: 'inst-b', allowed_actions: [actions.view] }
			]
		};
		const view = render(ConnectionSummary, { instance: instance({ state: 'running' }) });
		await click(screen.getByRole('button', { name: 'Check reachability' }));
		await vi.waitFor(() => expect(checks()).toHaveLength(1));

		await view.rerender({ instance: instance({ id: 'inst-b', state: 'running' }) });
		expect(screen.getByRole('button', { name: 'Check reachability' })).toHaveProperty(
			'disabled',
			false
		);
		answer();
		await new Promise((r) => setTimeout(r, 50));
		expect(screen.queryByRole('status')).toBeNull();
	});

	it('drops a result that arrives while the server is stopped', async () => {
		let answer!: () => void;
		daemon.on(
			'GET',
			'/instances/inst-a/reachability',
			() =>
				new Promise<Response>((resolve) => {
					answer = () => resolve(Response.json(report()));
				})
		);
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance({ state: 'running' }) });
		await click(screen.getByRole('button', { name: 'Check reachability' }));
		await vi.waitFor(() => expect(checks()).toHaveLength(1));

		await view.rerender({ instance: instance({ state: 'stopped' }) });
		answer();
		await new Promise((r) => setTimeout(r, 50));
		await view.rerender({ instance: instance({ state: 'running' }) });
		expect(screen.queryByRole('status')).toBeNull();
		expect(screen.getByRole('button', { name: 'Check reachability' })).toHaveProperty(
			'disabled',
			false
		);
	});

	it('disables the button while a check is running', async () => {
		let answer!: () => void;
		daemon.on(
			'GET',
			'/instances/inst-a/reachability',
			() =>
				new Promise<Response>((resolve) => {
					answer = () => resolve(Response.json(report()));
				})
		);
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		await click(screen.getByRole('button', { name: 'Check reachability' }));
		expect(screen.getByRole('button', { name: 'Checking…' })).toHaveProperty('disabled', true);
		answer();
		await screen.findByRole('status');
		expect(screen.getByRole('button', { name: 'Check reachability' })).toHaveProperty(
			'disabled',
			false
		);
	});

	it('forgets a result when the server stops', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance({ state: 'running' }) });
		await click(screen.getByRole('button', { name: 'Check reachability' }));
		await screen.findByRole('status');

		await view.rerender({ instance: instance({ state: 'stopped' }) });
		await view.rerender({ instance: instance({ state: 'running' }) });
		expect(screen.queryByRole('status')).toBeNull();
	});

	it('forgets a failure when the server stops', async () => {
		daemon.on('GET', '/instances/inst-a/reachability', () =>
			envelope(429, 'rate_limited', 'Too many requests. Try again shortly.')
		);
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance({ state: 'running' }) });
		await click(screen.getByRole('button', { name: 'Check reachability' }));
		await screen.findByText('Too many requests. Try again shortly.');

		await view.rerender({ instance: instance({ state: 'stopped' }) });
		await view.rerender({ instance: instance({ state: 'running' }) });
		expect(screen.queryByText('Too many requests. Try again shortly.')).toBeNull();
	});

	it('shows a passing verdict with no remedy', async () => {
		daemon.on('GET', '/instances/inst-a/reachability', () =>
			Response.json(
				report({
					status: 'ok',
					detail: 'The server answers at 203.0.113.7:2456. Players connect to that address.',
					remedy: undefined
				})
			)
		);
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		await click(screen.getByRole('button', { name: 'Check reachability' }));
		expect(await screen.findByText(/The server answers at 203\.0\.113\.7:2456/)).toBeTruthy();
		expect(screen.queryByText(/Forward UDP ports/)).toBeNull();
	});

	it('is absent while the server is stopped', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance({ state: 'stopped' }) });

		expect(screen.queryByRole('button', { name: 'Check reachability' })).toBeNull();
	});

	it('is absent without instance.view', () => {
		session.permissions = permissions('inst-a', []);
		render(ConnectionSummary, { instance: instance({ state: 'running' }) });

		expect(screen.queryByRole('button', { name: 'Check reachability' })).toBeNull();
		expect(checks()).toHaveLength(0);
	});
});
