import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AlertRule, Webhook } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/game-events') }
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function webhook(id: string, name: string): Webhook {
	return {
		id,
		name,
		kind: 'discord',
		enabled: true,
		created_at: '2026-09-01T00:00:00Z',
		updated_at: '2026-09-01T00:00:00Z'
	};
}

function rule(overrides: Partial<AlertRule>): AlertRule {
	return {
		id: 'rule-1',
		instance_id: 'inst-a',
		condition_kind: 'player_joined',
		params: {},
		quiet_start_minutes: null,
		quiet_end_minutes: null,
		quiet_timezone: null,
		enabled: true,
		webhook_ids: ['wh-1'],
		created_at: '2026-09-01T00:00:00Z',
		updated_at: '2026-09-01T00:00:00Z',
		...overrides
	};
}

function open({
	global = [actions.panelSettings],
	rules = [] as AlertRule[],
	hooks = [webhook('wh-1', 'Friends')],
	eye = null as string | null
} = {}) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(instance({ name: 'Midgard' })));
	daemon.on('GET', '/admin/webhooks', () => Response.json({ items: hooks, next_cursor: null }));
	daemon.on('GET', '/admin/alert-rules', () => Response.json({ items: rules, next_cursor: null }));
	daemon.on('GET', '/instances/inst-a/capabilities', () =>
		Response.json({
			command_channel: 'none',
			detected: true,
			allowed_commands: [],
			allowed_actions: [],
			world_tools: { upgrade_world: false, fresh_world: false },
			valmin_eye: eye
		})
	);
	session.permissions = permissions('inst-a', [actions.view], global);
	render(Page);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe("a server's game events", () => {
	it('is closed to anyone who does not manage notifications', () => {
		open({ global: [] });
		expect(text(document.body)).toContain('Only panel administrators can set up game events');
	});

	it('offers to install ValminEye when the server lacks it', async () => {
		open();
		const status = await screen.findByTestId('plugin-status');
		await vi.waitFor(() => expect(text(status)).toContain('needs the ValminEye plugin'));
		expect(
			within(status).getByRole('link', { name: 'Install ValminEye' }).getAttribute('href')
		).toBe('/instances/inst-a/mods?q=ValminEye');
	});

	it('links to the plugin settings when it is installed', async () => {
		open({ eye: '0.1.0' });
		const status = await screen.findByTestId('plugin-status');
		await vi.waitFor(() => expect(text(status)).toContain('ValminEye 0.1.0 is installed.'));
		expect(within(status).getByRole('link', { name: 'Plugin settings' }).getAttribute('href')).toBe(
			'/instances/inst-a/configs/io.valmin.eye.cfg'
		);
	});

	it('turns an event on with its message and previews it with this server', async () => {
		open();
		daemon.on('POST', '/admin/alert-rules', () =>
			Response.json(rule({ id: 'rule-new' }), { status: 201 })
		);
		await screen.findByRole('switch', { name: 'Player joined' });
		expect(screen.queryByLabelText('Message')).toBeNull();

		await click(screen.getByRole('switch', { name: 'Player joined' }));
		const row = screen.getByTestId('event-player_joined');
		expect(text(within(row).getByTestId('preview-player_joined'))).toContain('Alex joined');

		await fireEvent.input(within(row).getByLabelText('Message'), {
			target: { value: '{player} is on {server}' }
		});
		expect(text(within(row).getByTestId('preview-player_joined'))).toContain('Alex is on Midgard');

		await click(screen.getByRole('button', { name: 'Save changes' }));
		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'player_joined',
			instance_id: 'inst-a',
			webhook_ids: ['wh-1'],
			params: { template: '{player} is on {server}' }
		});
	});

	it('stores the default message as no template, and turns an event off by pausing its rule', async () => {
		open({ rules: [rule({ params: { template: 'custom' } })] });
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule({ enabled: false })));
		await screen.findByRole('switch', { name: 'Player joined' });

		await click(screen.getByRole('button', { name: /Use the default/ }));
		await click(screen.getByRole('switch', { name: 'Player joined' }));
		await click(screen.getByRole('button', { name: 'Save changes' }));

		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toEqual({
			enabled: false,
			params: {},
			webhook_ids: ['wh-1']
		});
		expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(0);
	});

	it('will not save an event that posts nowhere', async () => {
		open({ hooks: [webhook('wh-1', 'Friends'), webhook('wh-2', 'Raids')] });
		await screen.findByRole('switch', { name: 'Player joined' });

		await click(screen.getByRole('switch', { name: 'Player left' }));
		expect(text(document.body)).toContain('Choose at least one channel to post to.');
		expect(
			(screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled
		).toBe(true);
		await click(screen.getByRole('checkbox', { name: 'Raids' }));
		expect(
			(screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled
		).toBe(false);
	});
});
