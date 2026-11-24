import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { InstalledMod } from '#lib/api/mods.js';
import type { SavedSetup } from '#lib/api/setups.js';
import { FakeDaemon } from '#lib/testing/daemon.js';
import LastModChange from './last-mod-change.svelte';

const mod = { full_name: 'Author-Plugin', source: 'thunderstore', enabled: true } as const;

function setup(overrides: Partial<SavedSetup>): SavedSetup {
	return {
		id: 'auto-1',
		name: 'Before updating Author-Plugin',
		created_at: '2026-11-23T10:00:00Z',
		created_by: 'sam',
		game_build_id: '100',
		world_name: 'World',
		config_tree: true,
		configs_truncated: false,
		automatic: true,
		light: false,
		world_pending: false,
		mods: [{ ...mod, version: '1.0.0', side: 'unknown', locked: false, installed_as: 'explicit' }],
		configs_digest: 'd',
		...overrides
	};
}

const installed = [{ ...mod, version: '2.0.0' }] as InstalledMod[];

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('LastModChange', () => {
	it('names the newest automatic setup, what changed since and links to roll back to it', async () => {
		const current = setup({ id: 'manual', name: 'Mine', automatic: false }).mods.map((m) => ({
			...m,
			version: '2.0.0'
		}));
		daemon.on('GET', '/instances/inst-a/setups', () =>
			Response.json({
				items: [setup({ id: 'manual', name: 'Mine', automatic: false, mods: current }), setup({})]
			})
		);
		render(LastModChange, { instanceId: 'inst-a', installed });
		const line = await screen.findByTestId('last-mod-change');
		expect(line.textContent).toContain('Before updating Author-Plugin');
		expect(line.textContent).toContain('by sam');
		expect(line.textContent).toContain('since then: Author-Plugin 1.0.0 → 2.0.0');
		expect(screen.getByRole('link', { name: 'Undo' }).getAttribute('href')).toBe(
			'/instances/inst-a/setups?setup=auto-1'
		);
	});

	it('names a newer manual setup holding other mods, which stood in for the automatic one', async () => {
		daemon.on('GET', '/instances/inst-a/setups', () =>
			Response.json({
				items: [setup({ id: 'manual', name: 'Mine', automatic: false }), setup({ id: 'older' })]
			})
		);
		render(LastModChange, { instanceId: 'inst-a', installed });
		const line = await screen.findByTestId('last-mod-change');
		expect(line.textContent).toContain('Mine');
		expect(screen.getByRole('link', { name: 'Undo' }).getAttribute('href')).toBe(
			'/instances/inst-a/setups?setup=manual'
		);
	});

	it('shows nothing when the mods are as the setup holds them', async () => {
		daemon.on('GET', '/instances/inst-a/setups', () => Response.json({ items: [setup({})] }));
		render(LastModChange, {
			instanceId: 'inst-a',
			installed: [{ ...mod, version: '1.0.0' }] as InstalledMod[]
		});
		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/instances/inst-a/setups')).toHaveLength(1)
		);
		expect(screen.queryByTestId('last-mod-change')).toBeNull();
	});
});
