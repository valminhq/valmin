import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { HostPlayer } from '#lib/api/players.js';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, envelope, instance, permissions } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import Page from './+page.svelte';

let daemon: FakeDaemon;

const troll: HostPlayer = {
	platform_id: 'Steam_76561198000000001',
	names: [
		{ name: 'Troll', exact: true, hits: 0, likely: true },
		{ name: 'Newbald', exact: false, hits: 1, likely: false }
	],
	servers: [
		{
			instance_id: 'inst-a',
			instance_name: 'Home',
			name: 'Troll',
			first_seen_at: '2026-10-10T10:00:00Z',
			last_seen_at: '2026-10-11T10:00:00Z'
		},
		{
			instance_id: 'inst-b',
			instance_name: 'Modded',
			name: '',
			first_seen_at: '2026-10-09T10:00:00Z',
			last_seen_at: '2026-10-09T11:00:00Z'
		}
	],
	last_seen_at: '2026-10-11T10:00:00Z'
};

const alex: HostPlayer = {
	platform_id: 'Steam_76561198000000002',
	names: [{ name: 'Alex', exact: true, hits: 0, likely: true }],
	servers: [
		{
			instance_id: 'inst-a',
			instance_name: 'Home',
			name: 'Alex',
			first_seen_at: '2026-10-08T10:00:00Z',
			last_seen_at: '2026-10-08T11:00:00Z'
		}
	],
	last_seen_at: '2026-10-08T11:00:00Z'
};

async function open(global: string[]) {
	daemon.on('GET', '/players', () => Response.json({ items: [troll, alex], next_cursor: null }));
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: ['inst-a', 'inst-b', 'inst-c'].map((id) => instance({ id, name: id })),
			next_cursor: null
		})
	);
	session.permissions = permissions('', [], global);
	render(Page);
	if (global.includes(actions.playersManage)) await screen.findByText('Steam_76561198000000001');
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the players page', () => {
	it('is closed without players.manage and asks for nothing', async () => {
		await open([actions.view]);

		expect(screen.getByText(/do not have permission/)).toBeTruthy();
		expect(daemon.requests('GET', '/players')).toHaveLength(0);
	});

	it('lists each account once, with its names and the servers it was seen on', async () => {
		await open([actions.playersManage]);

		expect(screen.getByText('Troll')).toBeTruthy();
		expect(screen.getByText('maybe Newbald · 1×')).toBeTruthy();
		expect(screen.getByText(/on\s+Home, Modded/)).toBeTruthy();
		expect(screen.getByText('Steam_76561198000000002')).toBeTruthy();
	});

	it('finds an account by any of its names, its id, or a server it was seen on', async () => {
		await open([actions.playersManage]);
		const search = screen.getByLabelText('Search by name or ID');

		await fireEvent.input(search, { target: { value: 'newbald' } });
		expect(screen.getByText('Steam_76561198000000001')).toBeTruthy();
		expect(screen.queryByText('Steam_76561198000000002')).toBeNull();

		await fireEvent.input(search, { target: { value: '000000002' } });
		expect(screen.getByText('Steam_76561198000000002')).toBeTruthy();
		expect(screen.queryByText('Steam_76561198000000001')).toBeNull();

		await fireEvent.input(search, { target: { value: 'modded' } });
		expect(screen.getByText('Steam_76561198000000001')).toBeTruthy();
		expect(screen.queryByText('Steam_76561198000000002')).toBeNull();

		await fireEvent.input(search, { target: { value: 'nobody' } });
		expect(screen.getByText('No player matches.')).toBeTruthy();
	});
});

// Each server's ban list is written on its own, through its own conditional write.
describe('banning across servers', () => {
	const bans = (id: string) => daemon.requests('PUT', `/instances/${id}/bans`);

	function serve(id: string) {
		daemon.on('GET', `/instances/${id}/bans`, () =>
			Response.json({ ids: [] }, { headers: { ETag: `"${id}"` } })
		);
		daemon.on('PUT', `/instances/${id}/bans`, () =>
			Response.json({ ids: [troll.platform_id] }, { headers: { ETag: '"next"' } })
		);
	}

	/** Opens the ban dialog on Troll's row from the named button. */
	async function ask(button: string) {
		const row = screen.getByText(troll.platform_id).closest('li') as HTMLElement;
		await click(within(row).getByRole('button', { name: button }));
		return screen.findByRole('dialog');
	}

	it('bans on every server, reporting a failure without stopping the rest', async () => {
		await open([actions.playersManage]);
		serve('inst-a');
		serve('inst-c');
		daemon.on('GET', '/instances/inst-b/bans', () =>
			envelope(500, 'internal', 'Valmin could not read the list.')
		);

		const dialog = await ask('Ban on every server');
		await click(within(dialog).getByRole('button', { name: 'Ban on 3 servers' }));

		await vi.waitFor(() => expect(bans('inst-c')).toHaveLength(1));
		expect(bans('inst-a')[0].headers['If-Match']).toBe('"inst-a"');
		expect(bans('inst-a')[0].body).toEqual({ ids: [troll.platform_id] });
		expect(bans('inst-b')).toHaveLength(0);
		expect(within(dialog).getByText('Valmin could not read the list.')).toBeTruthy();
		expect(within(dialog).getAllByText('Banned')).toHaveLength(2);
	});

	it('starts the chosen-servers ban from the servers the account was seen on', async () => {
		await open([actions.playersManage]);
		serve('inst-a');
		serve('inst-b');

		const dialog = await ask('Ban on chosen servers');
		await click(within(dialog).getByRole('button', { name: 'Ban on 2 servers' }));

		await vi.waitFor(() => expect(bans('inst-b')).toHaveLength(1));
		expect(bans('inst-a')).toHaveLength(1);
		expect(daemon.requests('GET', '/instances/inst-c/bans')).toHaveLength(0);
	});
});
