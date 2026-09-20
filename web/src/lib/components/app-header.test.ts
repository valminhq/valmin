import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'svelte/server';
import { actions } from '$lib/api/instances';
import AppHeader from './app-header.svelte';

const state = vi.hoisted(() => ({ pathname: '/', granted: [] as string[] }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return new URL(state.pathname, 'http://localhost');
		}
	}
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/paths', () => ({ resolve: (route: string) => route }));
vi.mock('$lib/state/session.svelte', () => ({
	session: { allowedGlobally: () => state.granted, user: { username: 'ada' } }
}));
vi.mock('$lib/state/instances.svelte', () => ({ instanceList: { release: vi.fn() } }));
vi.mock('$lib/socket/index.svelte', () => ({ socketStatus: { value: 'open' } }));

beforeEach(() => {
	state.pathname = '/';
	state.granted = [];
});

describe('the panel header', () => {
	it('keeps the brand and the server list out of any menu', () => {
		const { body } = render(AppHeader);
		const brand = body.indexOf('Servers');
		expect(brand).toBeGreaterThan(-1);
		expect(body.slice(0, brand)).not.toContain('<details');
	});

	it('offers no administration menu without a single administrative action', () => {
		const { body } = render(AppHeader);
		expect(body).not.toContain('/admin/');
		expect(body.match(/<details/g), 'the account menu is the only one').toHaveLength(1);
	});

	it('collapses every granted destination into the administration menu', () => {
		state.granted = [actions.usersManage, actions.auditRead, actions.panelSettings];
		const { body } = render(AppHeader);
		const menu = body.slice(body.indexOf('Administration'));
		for (const href of ['/admin/users', '/admin/audit', '/admin/webhooks', '/admin/keys']) {
			expect(menu, `${href} is reachable`).toContain(`href="${href}"`);
		}
		expect(body, 'a destination nobody granted stays absent').not.toContain('/admin/invites');
	});

	it('names the open section on the menu that holds it, and marks the destination', () => {
		state.granted = [actions.auditRead];
		state.pathname = '/admin/audit';
		const { body } = render(AppHeader);
		expect(body).toMatch(/<summary[\s\S]*?Audit log[\s\S]*?<\/summary>/);
		expect(body).toMatch(/href="\/admin\/audit"[^>]*aria-current="page"/);
	});

	it('keeps the connection indicator outside both menus', () => {
		const { body } = render(AppHeader);
		expect(body, 'an open socket says nothing').not.toContain('role="status"');
	});
});
