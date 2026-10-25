import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { HostPlayer } from '$lib/api/players';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, permissions } from '$lib/testing/daemon';
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

	it('finds an account by any of its names or by its id', async () => {
		await open([actions.playersManage]);
		const search = screen.getByLabelText('Search by name or ID');

		await fireEvent.input(search, { target: { value: 'newbald' } });
		expect(screen.getByText('Steam_76561198000000001')).toBeTruthy();
		expect(screen.queryByText('Steam_76561198000000002')).toBeNull();

		await fireEvent.input(search, { target: { value: '000000002' } });
		expect(screen.getByText('Steam_76561198000000002')).toBeTruthy();
		expect(screen.queryByText('Steam_76561198000000001')).toBeNull();

		await fireEvent.input(search, { target: { value: 'nobody' } });
		expect(screen.getByText('No player matches.')).toBeTruthy();
	});
});
