import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'svelte/server';
import { actions, type Instance } from '$lib/api/instances';
import BackupsPanel from './backups-panel.svelte';
import ModsPage from '../../routes/instances/[id]/mods/+page.svelte';

const state = vi.hoisted(() => ({ allowed: [] as string[] }));
vi.mock('$app/state', () => ({ page: { params: { id: 'server-a' } } }));
vi.mock('$app/paths', () => ({
	resolve: (route: string, params?: { id: string }) => route.replace('[id]', params?.id ?? '')
}));
vi.mock('$lib/state/session.svelte', () => ({ session: { allowed: () => state.allowed } }));

const instance = {
	id: 'server-a',
	name: 'Friday night',
	world_name: 'Midgard',
	state: 'stopped'
} as Instance;
beforeEach(() => {
	state.allowed = [];
});

describe('backup task grouping', () => {
	it('does not reserve a settings column for a reader', () => {
		state.allowed = [actions.backupsList];
		const { body } = render(BackupsPanel, { props: { instance } });
		expect(body).toContain('Backup history');
		expect(body).not.toContain('<aside');
		expect(body).not.toContain('Save backup settings');
	});

	it('places retention controls in the settings column for an authorized editor', () => {
		state.allowed = [actions.backupsList, actions.settings];
		const { body } = render(BackupsPanel, { props: { instance } });
		expect(body).toMatch(/<aside[\s\S]*Save backup settings[\s\S]*<\/aside>/);
		expect(body).not.toContain('Add schedule');
	});

	it('keeps scheduling independent of backup-list and retention permissions', () => {
		state.allowed = [actions.restart];
		const { body } = render(BackupsPanel, { props: { instance } });
		expect(body).toContain('Add schedule');
		expect(body).not.toContain('Save backup settings');
		expect(body).toContain('Backups are not available to you.');
	});
});

it('opens Mods on Installed and exposes all three tasks as accessible tabs', () => {
	state.allowed = [actions.modsList];
	const { body } = render(ModsPage);
	expect(body).toContain('role="tablist"');
	expect(body.match(/role="tab"/g)).toHaveLength(3);
	expect(body.match(/aria-selected="true"/g)).toHaveLength(1);
	expect(body).toContain('Browse mods');
	expect(body).toContain('Player modpack');
});
