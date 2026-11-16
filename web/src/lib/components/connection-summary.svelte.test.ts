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

		expect(screen.getByRole('button', { name: 'Show password' })).toBeTruthy();
		expect(screen.queryByText('hunter22')).toBeNull();
		expect(reads()).toHaveLength(0);
	});

	it('reads the password once on Show, offers a copy, and forgets it on Hide', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance() });

		await click(screen.getByRole('button', { name: 'Show password' }));
		const value = await screen.findByText('hunter22');
		expect(value.tagName).toBe('CODE');
		expect(screen.getByRole('button', { name: 'Copy password' })).toBeTruthy();
		expect(reads()).toHaveLength(1);

		await click(screen.getByRole('button', { name: 'Hide' }));
		expect(screen.queryByText('hunter22')).toBeNull();
		expect(screen.getByRole('button', { name: 'Show password' })).toBeTruthy();
		expect(reads()).toHaveLength(1);
	});

	it('shows the daemon error inline when the read fails', async () => {
		daemon.on('GET', '/instances/inst-a/password', () =>
			envelope(500, 'internal', 'Something went wrong.')
		);
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance() });

		await click(screen.getByRole('button', { name: 'Show password' }));
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
		await click(screen.getByRole('button', { name: 'Show password' }));
		await screen.findByText('hunter22');

		await view.rerender({ instance: instance({ id: 'inst-b' }) });
		expect(screen.queryByText('hunter22')).toBeNull();
		expect(reads('inst-b')).toHaveLength(0);
	});

	it('keeps a revealed password when the same instance is updated', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance() });
		await click(screen.getByRole('button', { name: 'Show password' }));
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
		await click(screen.getByRole('button', { name: 'Show password' }));
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
		expect(screen.queryByRole('button', { name: 'Show password' })).toBeNull();
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
		expect(screen.getByRole('button', { name: 'Copy public address' })).toBeTruthy();
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
		expect(screen.queryByRole('button', { name: 'Copy public address' })).toBeNull();
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

describe('the connect step', () => {
	it('offers the join address, a copy and the Steam link', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, {
			instance: instance({
				join_host: 'play.example.com',
				join_address: 'play.example.com:2456',
				steam_join_url: 'steam://run/892970//+connect%20play.example.com:2456'
			})
		});

		expect(screen.getByText('play.example.com:2456').tagName).toBe('CODE');
		expect(screen.getByRole('button', { name: 'Copy join address' })).toBeTruthy();
		const steam = screen.getByRole('link', { name: 'Join with Steam' });
		expect(steam.getAttribute('href')).toBe('steam://run/892970//+connect%20play.example.com:2456');
		expect(screen.getByText(/Close the game before using Join with Steam/)).toBeTruthy();
	});

	it('shows the port and points to Settings when no address is set', () => {
		session.permissions = permissions('inst-a', [actions.view, actions.settings]);
		render(ConnectionSummary, { instance: instance() });

		expect(screen.getByText(/No join address is set/)).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Settings' })).toBeTruthy();
		expect(screen.queryByRole('link', { name: 'Join with Steam' })).toBeNull();
	});

	it('does not point a viewer to Settings', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, { instance: instance() });

		expect(screen.getByText(/No join address is set/)).toBeTruthy();
		expect(screen.queryByRole('link', { name: 'Settings' })).toBeNull();
	});

	it('shows the join code instead of the address on a crossplay server', () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(ConnectionSummary, {
			instance: instance({
				crossplay: true,
				crossplay_join_code: '123456',
				join_host: 'play.example.com',
				join_address: 'play.example.com:2456'
			})
		});

		expect(screen.getByText('123456')).toBeTruthy();
		expect(screen.queryByText('play.example.com:2456')).toBeNull();
		expect(screen.queryByRole('link', { name: 'Join with Steam' })).toBeNull();
	});
});

describe('the mods step', () => {
	const previews = () => daemon.requests('GET', '/instances/inst-a/mods/export');
	const uploads = () => daemon.requests('POST', '/instances/inst-a/mods/export/code');
	const preview = (over: Record<string, unknown> = {}) => ({
		profile_name: 'inst-a',
		mods: [
			{
				full_name: 'a-One',
				version: '1.0.0',
				source: 'thunderstore',
				side: 'client_required',
				reason: 'tagged'
			},
			{
				full_name: 'b-Two',
				version: '2.0.0',
				source: 'thunderstore',
				side: 'client_required',
				reason: 'tagged'
			}
		],
		excluded: [],
		conflicts: [],
		...over
	});

	beforeEach(() => {
		daemon.on('GET', '/instances/inst-a/mods/export', () => Response.json(preview()));
		daemon.on('POST', '/instances/inst-a/mods/export/code', () =>
			Response.json({ code: '01234567-89ab-cdef-0123-456789abcdef', registry: 'thunderstore' })
		);
	});

	it('lists the mods and uploads nothing until asked', async () => {
		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		render(ConnectionSummary, { instance: instance({ modded: true }) });

		expect(await screen.findByText(/Players need 2 mods/)).toBeTruthy();
		expect(screen.getByRole('button', { name: 'Create profile code' })).toBeTruthy();
		expect(uploads()).toHaveLength(0);
	});

	it('creates a profile code on request and offers a copy', async () => {
		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		render(ConnectionSummary, { instance: instance({ modded: true }) });

		await click(await screen.findByRole('button', { name: 'Create profile code' }));
		expect(await screen.findByText('01234567-89ab-cdef-0123-456789abcdef')).toBeTruthy();
		expect(screen.getByRole('button', { name: 'Copy profile code' })).toBeTruthy();
		expect(screen.queryByText('Works in Gale only.')).toBeNull();
		expect(uploads()).toHaveLength(1);
	});

	it('notes that a Hexium code works in Gale only', async () => {
		daemon.on('POST', '/instances/inst-a/mods/export/code', () =>
			Response.json({ code: '01234567-89ab-cdef-0123-456789abcdef', registry: 'hexium' })
		);
		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		render(ConnectionSummary, { instance: instance({ modded: true }) });

		await click(await screen.findByRole('button', { name: 'Create profile code' }));
		expect(await screen.findByText('Works in Gale only.')).toBeTruthy();
	});

	it('sends a conflict to the Player modpack tab instead of offering a code', async () => {
		daemon.on('GET', '/instances/inst-a/mods/export', () =>
			Response.json(
				preview({
					conflicts: [
						{ full_name: 'c-Three', version: '1.0.0', required_by: 'a-One', side: 'server_only' }
					]
				})
			)
		);
		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		render(ConnectionSummary, { instance: instance({ modded: true }) });

		expect(await screen.findByRole('link', { name: 'Player modpack' })).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Create profile code' })).toBeNull();
	});

	it('is absent without mods.list, and on a server without mods', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		const view = render(ConnectionSummary, { instance: instance({ modded: true }) });
		await new Promise((r) => setTimeout(r, 0));
		expect(screen.queryByText(/Install the mods/)).toBeNull();
		view.unmount();

		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		render(ConnectionSummary, { instance: instance({ modded: false }) });
		await new Promise((r) => setTimeout(r, 0));
		expect(screen.queryByText(/Install the mods/)).toBeNull();
		expect(previews()).toHaveLength(0);
	});

	it('keeps the step and reads the list once while the same server changes state', async () => {
		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		const view = render(ConnectionSummary, { instance: instance({ modded: true }) });
		await screen.findByText(/Players need 2 mods/);

		await view.rerender({ instance: instance({ modded: true, state: 'starting' }) });
		await view.rerender({ instance: instance({ modded: true, state: 'running' }) });
		expect(screen.getByText(/Players need 2 mods/)).toBeTruthy();
		expect(previews()).toHaveLength(1);
	});

	it('is absent when no mod is labelled for players', async () => {
		daemon.on('GET', '/instances/inst-a/mods/export', () => Response.json(preview({ mods: [] })));
		session.permissions = permissions('inst-a', [actions.view, actions.modsList]);
		render(ConnectionSummary, { instance: instance({ modded: true }) });
		await vi.waitFor(() => expect(previews()).toHaveLength(1));
		await new Promise((r) => setTimeout(r, 0));
		expect(screen.queryByText(/Install the mods/)).toBeNull();
	});
});
