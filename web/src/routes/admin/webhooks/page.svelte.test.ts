import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AlertRule, Delivery, Webhook } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance, permissions } from '$lib/testing/daemon';
import { choose, click, text } from '$lib/testing/interact';
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

function rule(overrides: Partial<AlertRule> = {}): AlertRule {
	return {
		id: 'rule-1',
		instance_id: null,
		condition_kind: 'crash_loop',
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

async function open(
	global: string[],
	deliveries: Delivery[] = [],
	rules: AlertRule[] = [],
	destinations: Webhook[] = [ops]
) {
	daemon.on('GET', '/admin/webhooks', () =>
		Response.json({ items: destinations, next_cursor: null })
	);
	daemon.on('GET', '/admin/webhooks/deliveries', () =>
		Response.json({ items: deliveries, next_cursor: null })
	);
	daemon.on('GET', '/admin/alert-rules', () => Response.json({ items: rules, next_cursor: null }));
	daemon.on('GET', '/instances', () =>
		Response.json({ items: [instance({ id: 'inst-a', name: 'Midgard' })], next_cursor: null })
	);
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

// ADR-167, 11 §9: a destination URL is a credential the panel takes once and never shows
// again, and the address policy is stated where the URL is typed.
describe('the notifications screen', () => {
	it('is closed without panel.settings', async () => {
		await open([actions.create]);
		expect(screen.getByText('Notification settings are not available to you.')).toBeTruthy();
		expect(screen.queryByLabelText('URL')).toBeNull();
	});

	it('says what is sent, what is not, and that the URL is never shown again', async () => {
		await open([actions.panelSettings]);
		await screen.findAllByText('Ops channel');
		const page = text(document.body);
		expect(page).toContain('A stop you asked for is not a notification.');
		expect(page).toContain('stores it encrypted and never shows it again');
		expect(page).toContain(
			'Addresses on this machine or its network are refused, and the panel does not follow redirects.'
		);
	});

	it('adds a destination and never quotes a URL back', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/webhooks', () => Response.json(ops, { status: 201 }));
		await screen.findAllByText('Ops channel');

		await fireEvent.input(screen.getByLabelText('Name'), { target: { value: ' Raid alerts ' } });
		await fireEvent.input(screen.getByLabelText('URL'), {
			target: { value: 'https://discord.com/api/webhooks/1/secret-token' }
		});
		await click(screen.getByRole('button', { name: 'Add destination' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/webhooks')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/webhooks')[0].body).toEqual({
			name: 'Raid alerts',
			kind: 'discord',
			url: 'https://discord.com/api/webhooks/1/secret-token'
		});
		await vi.waitFor(() =>
			expect((screen.getByLabelText('URL') as HTMLInputElement).value, 'the field is cleared').toBe(
				''
			)
		);
		expect(text(document.body)).not.toContain('secret-token');
	});

	it('shows the daemon’s refusal of an address', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/webhooks', () =>
			envelope(422, 'validation_failed', 'That address is on a private network.')
		);
		await screen.findAllByText('Ops channel');

		await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'LAN' } });
		await fireEvent.input(screen.getByLabelText('URL'), {
			target: { value: 'https://10.0.0.5/hook' }
		});
		await click(screen.getByRole('button', { name: 'Add destination' }));
		expect(await screen.findByText(/That address is on a private network\./)).toBeTruthy();
	});

	it('deletes a destination only after its name is typed back', async () => {
		await open([actions.panelSettings]);
		daemon.on('DELETE', '/admin/webhooks/wh-1', () => new Response(null, { status: 204 }));
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('button', { name: 'Delete destination' }));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('the URL cannot be recovered');
		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'Ops channel' }
		});
		await click(within(dialog).getByRole('button', { name: 'Delete destination' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/admin/webhooks/wh-1')).toHaveLength(1)
		);
	});

	it('shows a failed delivery with its reason', async () => {
		await open(
			[actions.panelSettings],
			[
				{
					id: 'd-1',
					webhook_id: 'wh-1',
					event_id: 'e-1',
					event_kind: 'backup_failed',
					instance_id: 'inst-a',
					status: 'failed',
					attempts: 3,
					last_error: 'The destination answered 404.',
					created_at: '2026-09-24T10:00:00Z',
					updated_at: '2026-09-24T10:02:00Z'
				}
			]
		);

		expect(await screen.findByText('The destination answered 404.')).toBeTruthy();
		expect(text(document.body)).toContain('backup failed → Ops channel');
		expect(text(document.body)).toContain('3 attempts');
	});
});

describe('the alert rules card', () => {
	it('lists each rule with its condition, server and destinations', async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule(),
				rule({
					id: 'rule-2',
					condition_kind: 'stale_backup',
					instance_id: 'inst-a',
					quiet_start_minutes: 1320,
					quiet_end_minutes: 420,
					quiet_timezone: 'Europe/Berlin',
					params: { stale_factor: 3 },
					webhook_ids: []
				})
			]
		);
		const first = await screen.findByTestId('alert-rule-rule-1');
		expect(text(first)).toContain('Crash loop');
		expect(text(first)).toContain('Every server');
		expect(text(first)).toContain('Ops channel');

		const second = screen.getByTestId('alert-rule-rule-2');
		expect(text(second)).toContain('Backups stale');
		expect(text(second)).toContain('Midgard');
		expect(text(second)).toContain('quiet hours');
		expect(text(second)).toContain('custom thresholds');
		expect(text(second)).toContain('sends nothing');
	});

	it('asks for a destination first when there is none', async () => {
		await open([actions.panelSettings], [], [], []);
		expect(await screen.findByText(/Add a destination above before adding a rule\./)).toBeTruthy();
	});

	it('keeps Add disabled until a destination is ticked, then posts the rule', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		const add = screen.getByRole('button', { name: 'Add rule' }) as HTMLButtonElement;
		expect(add.disabled).toBe(true);

		await choose(screen.getByLabelText('Condition'), 'Job stuck');
		await choose(screen.getByLabelText('Server'), 'Midgard');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		expect(add.disabled).toBe(false);
		await click(add);

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'job_stuck',
			instance_id: 'inst-a',
			webhook_ids: ['wh-1']
		});
	});

	it('posts a low disk rule for every server, since low disk is host-wide', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		await choose(screen.getByLabelText('Server'), 'Midgard');
		await choose(screen.getByLabelText('Condition'), 'Low disk');
		const server = screen.getByLabelText('Server') as HTMLButtonElement;
		expect(server.disabled).toBe(true);
		expect(text(server)).toContain('Every server');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'low_disk',
			instance_id: null,
			webhook_ids: ['wh-1']
		});
	});

	it('does not count a ticked destination that was then deleted', async () => {
		const raid: Webhook = { ...ops, id: 'wh-2', name: 'Raid alerts' };
		await open([actions.panelSettings], [], [], [ops, raid]);
		daemon.on('DELETE', '/admin/webhooks/wh-1', () => {
			daemon.on('GET', '/admin/webhooks', () =>
				Response.json({ items: [raid], next_cursor: null })
			);
			return new Response(null, { status: 204 });
		});
		await screen.findAllByText('Ops channel');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		const add = screen.getByRole('button', { name: 'Add rule' }) as HTMLButtonElement;
		expect(add.disabled).toBe(false);

		await click(screen.getAllByRole('button', { name: 'Delete destination' })[0]);
		const dialog = await screen.findByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'Ops channel' }
		});
		await click(within(dialog).getByRole('button', { name: 'Delete destination' }));
		await vi.waitFor(() =>
			expect(screen.queryByRole('checkbox', { name: 'Ops channel' })).toBeNull()
		);
		expect(add.disabled).toBe(true);
	});

	it('sends only enabled when a rule is switched off', async () => {
		await open([actions.panelSettings], [], [rule()]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule({ enabled: false })));
		const row = await screen.findByTestId('alert-rule-rule-1');

		await click(within(row).getByRole('switch'));
		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toEqual({
			enabled: false
		});
	});

	it('deletes a rule after confirming', async () => {
		await open([actions.panelSettings], [], [rule()]);
		daemon.on('DELETE', '/admin/alert-rules/rule-1', () => new Response(null, { status: 204 }));
		const row = await screen.findByTestId('alert-rule-rule-1');

		await click(within(row).getByRole('button', { name: 'Delete rule' }));
		const dialog = await screen.findByRole('dialog');
		expect(daemon.requests('DELETE', '/admin/alert-rules/rule-1')).toHaveLength(0);
		await click(within(dialog).getByRole('button', { name: 'Delete rule' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
	});
});
