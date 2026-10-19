import { render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AlertRule, Webhook } from '$lib/api/notifications';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: {
		params: { id: 'inst-a' },
		url: new URL('http://localhost/instances/inst-a/notifications')
	}
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function webhook(id: string, name: string, owner: string | null): Webhook {
	return {
		id,
		name,
		kind: 'discord',
		enabled: true,
		instance_id: owner,
		catch_all: true,
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
		webhook_ids: ['wh-own'],
		created_at: '2026-09-01T00:00:00Z',
		updated_at: '2026-09-01T00:00:00Z',
		...overrides
	};
}

const base = '/instances/inst-a';

function open({
	held = [actions.view, actions.notificationsManage],
	rules = [] as AlertRule[],
	hooks = [webhook('wh-own', 'Friends', 'inst-a'), webhook('wh-shared', 'Ops', null)],
	eye = null as string | null
} = {}) {
	daemon.on('GET', base, () => Response.json(instance({ name: 'Midgard' })));
	daemon.on('GET', `${base}/webhooks`, () => Response.json({ items: hooks, next_cursor: null }));
	daemon.on('GET', `${base}/webhooks/deliveries`, () =>
		Response.json({ items: [], next_cursor: null })
	);
	daemon.on('GET', `${base}/alert-rules`, () => Response.json({ items: rules, next_cursor: null }));
	daemon.on('GET', `${base}/capabilities`, () =>
		Response.json({
			command_channel: 'none',
			detected: true,
			allowed_commands: [],
			allowed_actions: [],
			world_tools: { upgrade_world: false, fresh_world: false },
			valmin_eye: eye,
			packages: {
				rcon: 'Tristan-ValheimRcon',
				upgrade_world: 'JereKuusela-Upgrade_World',
				fresh_world: 'sighsorry-FreshWorld'
			}
		})
	);
	session.permissions = permissions('inst-a', held);
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

describe("a server's notifications", () => {
	it('is closed without notifications.manage on the server', () => {
		open({ held: [actions.view] });
		expect(
			screen.getByText('Managing this server’s notifications is not available to you.')
		).toBeTruthy();
		expect(daemon.requests('GET', `${base}/webhooks`)).toHaveLength(0);
	});

	it('manages its own channels and lists the shared ones read-only', async () => {
		open();
		const own = await screen.findByTestId('channel-wh-own');
		const shared = screen.getByTestId('channel-wh-shared');
		expect(within(own).getByRole('button', { name: 'Delete Friends' })).toBeTruthy();
		expect(text(shared)).toContain('shared');
		expect(text(shared)).toContain('Managed in Administration');
		expect(within(shared).queryByRole('button')).toBeNull();
	});

	it('lists its own rules and the panel-wide ones it cannot change', async () => {
		open({
			rules: [
				rule({}),
				rule({ id: 'rule-2', instance_id: null, condition_kind: 'crash_loop' }),
				rule({ id: 'rule-3', instance_id: null, condition_kind: 'low_disk' })
			]
		});
		const own = await screen.findByTestId('rule-rule-1');
		expect(within(own).getByRole('button', { name: 'Edit the Player joined rule' })).toBeTruthy();

		const panel = screen.getByTestId('panel-rules');
		const wide = within(panel).getByTestId('rule-rule-2');
		expect(within(wide).queryByRole('button', { name: /Edit/ })).toBeNull();
		expect((within(wide).getByRole('switch') as HTMLButtonElement).disabled).toBe(true);
		expect(text(panel)).toContain('A panel administrator manages these.');
		expect(
			within(panel).queryByTestId('rule-rule-3'),
			'a host-wide rule is not about it'
		).toBeNull();
	});

	it('adds a rule about this server, without the host-wide events', async () => {
		open();
		daemon.on('POST', `${base}/alert-rules`, () => Response.json(rule({}), { status: 201 }));
		await screen.findByTestId('channel-wh-own');

		await click(screen.getByRole('button', { name: 'Add rule' }));
		const dialog = await screen.findByRole('dialog');
		expect(within(dialog).queryByTestId('pick-low_disk')).toBeNull();
		expect(within(dialog).queryByTestId('pick-power_cut')).toBeNull();
		expect(text(dialog)).toContain('These need the ValminEye plugin on the server.');
		await click(within(dialog).getByTestId('pick-player_joined'));
		expect(text(dialog), 'shared channels are marked').toContain('shared');
		await click(within(dialog).getByRole('checkbox', { name: /Friends/ }));
		await click(within(dialog).getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', `${base}/alert-rules`)).toHaveLength(1));
		const body = daemon.requests('POST', `${base}/alert-rules`)[0].body as Record<string, unknown>;
		expect(body.condition_kind).toBe('player_joined');
		expect(body.webhook_ids).toEqual(['wh-own']);
		expect(body, 'the route names the server').not.toHaveProperty('instance_id');
	});

	it('asks for the plugin once a game event is set up without it', async () => {
		open({ rules: [rule({})] });
		const card = await screen.findByTestId('plugin-status');
		await vi.waitFor(() => expect(text(card)).toContain('Game events need the ValminEye plugin'));
	});

	it('shows no plugin card before any game event is set up', async () => {
		open();
		await screen.findByTestId('channel-wh-own');
		expect(screen.queryByTestId('plugin-status')).toBeNull();
	});
});
