import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AlertRule, Webhook } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

const ops: Webhook = {
	id: 'wh-1',
	name: 'Ops channel',
	kind: 'discord',
	enabled: true,
	created_at: '2026-09-01T00:00:00Z',
	updated_at: '2026-09-01T00:00:00Z'
};

const crashRule: AlertRule = {
	id: 'rule-crash',
	instance_id: null,
	condition_kind: 'crash_loop',
	params: {},
	quiet_start_minutes: null,
	quiet_end_minutes: null,
	quiet_timezone: null,
	enabled: true,
	webhook_ids: ['wh-1'],
	created_at: '2026-09-01T00:00:00Z',
	updated_at: '2026-09-01T00:00:00Z'
};

function open(global: string[]) {
	daemon.on('GET', '/admin/webhooks', () => Response.json({ items: [ops], next_cursor: null }));
	daemon.on('GET', '/admin/alert-rules', () =>
		Response.json({ items: [crashRule], next_cursor: null })
	);
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: [
				instance({ id: 'inst-a', name: 'Midgard' }),
				instance({ id: 'inst-b', name: 'Ashlands' })
			],
			next_cursor: null
		})
	);
	for (const [id, version] of [
		['inst-a', '0.1.0'],
		['inst-b', null]
	] as const) {
		daemon.on('GET', `/instances/${id}/capabilities`, () =>
			Response.json({
				command_channel: 'none',
				detected: true,
				allowed_commands: [],
				allowed_actions: [],
				world_tools: { upgrade_world: false, fresh_world: false },
				valmin_eye: version
			})
		);
	}
	session.permissions = permissions('', [], global);
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

describe('the game events screen', () => {
	it('is closed without panel.settings', () => {
		open([actions.create]);
		expect(screen.getByText('Notification settings are not available to you.')).toBeTruthy();
	});

	it('shows which servers have ValminEye, with install and configure links', async () => {
		open([actions.panelSettings]);
		const installed = await screen.findByTestId('eye-inst-a');
		await vi.waitFor(() => expect(text(installed)).toContain('ValminEye 0.1.0'));
		expect(installed.querySelector('a')?.getAttribute('href')).toBe(
			'/instances/inst-a/configs/io.valmin.eye.cfg'
		);
		const missing = screen.getByTestId('eye-inst-b');
		await vi.waitFor(() => expect(text(missing)).toContain('Not installed'));
		expect(missing.querySelector('a')?.getAttribute('href')).toBe(
			'/instances/inst-b/mods?q=ValminEye'
		);
	});

	it('lists only game event rules', async () => {
		open([actions.panelSettings]);
		await screen.findByText('No game event rules yet.');
		expect(screen.queryByTestId('game-rule-rule-crash')).toBeNull();
	});

	it('previews the template and saves it on the rule', async () => {
		open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () =>
			Response.json(
				{ ...crashRule, id: 'rule-join', condition_kind: 'player_joined' },
				{ status: 201 }
			)
		);
		await screen.findByText('Ops channel');

		expect(text(screen.getByTestId('game-preview'))).toContain('Alex joined');
		await fireEvent.input(screen.getByLabelText('Message'), {
			target: { value: '{player} is on {server}' }
		});
		expect(text(screen.getByTestId('game-preview'))).toContain('Alex is on Vikings');

		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'player_joined',
			instance_id: null,
			webhook_ids: ['wh-1'],
			params: { template: '{player} is on {server}' },
			quiet_start_minutes: 0,
			quiet_end_minutes: 0,
			quiet_timezone: ''
		});
	});
});
