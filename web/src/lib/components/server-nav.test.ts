import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'svelte/server';
import { actions } from '$lib/api/instances';
import ServerNav from './server-nav.svelte';

const state = vi.hoisted(() => ({ pathname: '/instances/server-a', allowed: [] as string[] }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return new URL(state.pathname, 'http://localhost');
		}
	}
}));
vi.mock('$app/paths', () => ({
	resolve: (route: string, params: { id: string }) => route.replace('[id]', params.id)
}));
vi.mock('$lib/state/session.svelte', () => ({ session: { allowed: () => state.allowed } }));

beforeEach(() => {
	state.pathname = '/instances/server-a';
	state.allowed = [];
});

describe('server navigation', () => {
	it('keeps read-only settings and overview reachable without management permissions', () => {
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		expect(body).toContain('Overview');
		expect(body).toContain('Server settings');
		expect(body).not.toContain('Backups');
		expect(body).not.toContain('Panel access');
		expect(body).not.toContain('Player access');
		expect(body).not.toContain('Maintenance');
		expect(body).not.toContain('Compare');
	});

	it.each([
		{ held: [actions.backupsCreate], visible: true },
		{ held: [actions.restart], visible: true },
		{ held: [actions.gameUpdate], visible: true },
		{ held: [actions.backupsList, actions.view], visible: false }
	])('offers Maintenance to a member who may schedule a kind ($held)', ({ held, visible }) => {
		state.allowed = held;
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		expect(body.includes('href="/instances/server-a/maintenance"')).toBe(visible);
	});

	it('lists Maintenance right after Backups', () => {
		state.allowed = [actions.backupsList, actions.modsList, actions.backupsCreate];
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		const backups = body.indexOf('href="/instances/server-a/backups"');
		const maintenance = body.indexOf('href="/instances/server-a/maintenance"');
		const mods = body.indexOf('href="/instances/server-a/mods"');
		expect(backups).toBeGreaterThan(-1);
		expect(maintenance).toBeGreaterThan(backups);
		expect(mods).toBeGreaterThan(maintenance);
	});

	it.each([
		{ held: [actions.settings, actions.modsList, actions.configRead], visible: true },
		{ held: [actions.modsList, actions.configRead], visible: false },
		{ held: [actions.settings, actions.configRead], visible: false },
		{ held: [actions.settings, actions.modsList], visible: false }
	])('offers Compare only with settings, mods and settings files ($held)', ({ held, visible }) => {
		state.allowed = held;
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		expect(body.includes('href="/instances/server-a/compare"')).toBe(visible);
		if (visible) {
			expect(body.indexOf('href="/instances/server-a/compare"')).toBeLessThan(
				body.indexOf('href="/instances/server-a/settings"')
			);
		}
	});

	it('offers only the server sections authorized by their capabilities', () => {
		state.allowed = [actions.backupsList, actions.modsList];
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		expect(body).toContain('href="/instances/server-a/backups"');
		expect(body).toContain('href="/instances/server-a/mods"');
		expect(body).not.toContain('href="/instances/server-a/configs"');
		expect(body).not.toContain('href="/instances/server-a/access"');
	});

	it('marks the settings-files section active on a nested file page', () => {
		state.allowed = [actions.configRead];
		state.pathname = '/instances/server-a/configs/plugin.cfg';
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		expect(body).toMatch(/href="\/instances\/server-a\/configs"[^>]*aria-current="page"/);
		// The wide row and the narrow menu both render; only one is ever displayed, and they
		// agree because the same predicate marks both.
		expect(body.match(/aria-current="page"/g)).toHaveLength(2);
	});

	it('names the current section on the narrow-width selector', () => {
		state.allowed = [actions.backupsList];
		state.pathname = '/instances/server-a/backups';
		const { body } = render(ServerNav, { props: { id: 'server-a' } });
		expect(body).toContain('<details');
		expect(body).toMatch(/<summary[\s\S]*?Backups[\s\S]*?<\/summary>/);
	});
});
