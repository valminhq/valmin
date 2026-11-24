import { render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CacheInventory } from '#lib/api/admin.js';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, job, permissions } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import Page from './+page.svelte';

vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

const GiB = 1024 ** 3;

const inventory: CacheInventory = {
	items: [
		{
			kind: 'game_build',
			name: '21981590',
			registry: null,
			size_bytes: GiB,
			reclaimable_bytes: 0,
			in_use: true,
			prunable: false,
			servers: [{ id: 'inst-a', name: 'Viking World' }],
			setups: 0,
			prunable_at: null
		},
		{
			kind: 'mod_archive',
			name: 'Old-Mod-1.0.0',
			registry: 'thunderstore',
			size_bytes: 2 * 1024 ** 2,
			reclaimable_bytes: 2 * 1024 ** 2,
			in_use: false,
			prunable: true,
			servers: [],
			setups: 0,
			prunable_at: '2026-11-24T10:00:00Z'
		}
	],
	next_cursor: null,
	total: null,
	summary: [
		{ kind: 'mod_archive', count: 1, size_bytes: 2 * 1024 ** 2, reclaimable_bytes: 2 * 1024 ** 2 },
		{ kind: 'game_build', count: 1, size_bytes: GiB, reclaimable_bytes: 0 },
		{ kind: 'setup_blob', count: 0, size_bytes: 0, reclaimable_bytes: 0 },
		{ kind: 'partial_download', count: 0, size_bytes: 0, reclaimable_bytes: 0 }
	],
	grace_seconds: 3600
};

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/admin/cache', () => Response.json(inventory));
	daemon.on('POST', '/admin/cache/prune', () =>
		Response.json(job({ kind: 'cache_prune' }), { status: 202 })
	);
	daemon.on('GET', '/jobs/job-1', () =>
		Response.json(job({ kind: 'cache_prune', status: 'succeeded', message: 'freed 2.0 MiB' }))
	);
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the cache screen', () => {
	it('shows nothing and asks nothing without panel.settings', () => {
		session.permissions = permissions('', [], [actions.create]);
		render(Page);
		expect(screen.queryByRole('button', { name: /Prune cache/ })).toBeNull();
		expect(daemon.requests('GET', '/admin/cache')).toEqual([]);
	});

	it('lists unused items first and everything on request', async () => {
		session.permissions = permissions('', [], [actions.panelSettings]);
		render(Page);

		expect(await screen.findByText(/Pruning now frees 2\.0 MiB/)).toBeTruthy();
		expect(screen.getByText('Old-Mod-1.0.0')).toBeTruthy();
		expect(screen.getByText(/Unused, frees 2\.0 MiB/)).toBeTruthy();
		expect(screen.queryByText(/Used by Viking World/)).toBeNull();

		await click(screen.getByRole('switch', { name: 'Only unused items' }));
		expect(await screen.findByText(/Used by Viking World/)).toBeTruthy();
	});

	it('prunes as a job after a confirmation and reads the cache again', async () => {
		session.permissions = permissions('', [], [actions.panelSettings]);
		render(Page);

		await click(await screen.findByRole('button', { name: /Prune cache/ }));
		expect(daemon.requests('POST', '/admin/cache/prune')).toEqual([]);
		await click(
			within(await screen.findByRole('dialog')).getByRole('button', { name: 'Prune cache' })
		);

		expect(await screen.findByText(/freed 2\.0 MiB/i)).toBeTruthy();
		expect(daemon.requests('POST', '/admin/cache/prune')).toHaveLength(1);
		await vi.waitFor(() => expect(daemon.requests('GET', '/admin/cache').length).toBe(2));
	});
});
