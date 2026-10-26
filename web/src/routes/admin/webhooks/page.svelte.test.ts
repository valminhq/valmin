import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AlertRule, Delivery, DeliveryPage, Webhook } from '#lib/api/notifications.js';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, envelope, instance, permissions } from '#lib/testing/daemon.js';
import { choose, click, text } from '#lib/testing/interact.js';
import Page from './+page.svelte';

vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

let daemon: FakeDaemon;

function webhook(overrides: Partial<Webhook> = {}): Webhook {
	return {
		id: 'wh-1',
		name: 'Ops channel',
		kind: 'discord',
		enabled: true,
		instance_id: null,
		catch_all: true,
		created_at: '2026-09-01T00:00:00Z',
		updated_at: '2026-09-01T00:00:00Z',
		...overrides
	};
}
const ops = webhook();

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

function delivery(overrides: Partial<Delivery> = {}): Delivery {
	return {
		id: 'd-1',
		webhook_id: 'wh-1',
		event_id: 'e-1',
		event_kind: 'backup_failed',
		instance_id: null,
		rule_id: null,
		status: 'delivered',
		attempts: 1,
		last_error: null,
		created_at: '2026-09-24T10:00:00Z',
		updated_at: '2026-09-24T10:00:00Z',
		...overrides
	};
}

/** The delivery list as a fixed page, or as an answer to each request's query. */
type Deliveries = Delivery[] | ((query: URLSearchParams) => DeliveryPage);

async function open(
	global: string[],
	deliveries: Deliveries = [],
	rules: AlertRule[] = [],
	channels: Webhook[] = [ops]
) {
	daemon.on('GET', '/admin/webhooks', () => Response.json({ items: channels, next_cursor: null }));
	daemon.on('GET', '/admin/webhooks/deliveries', ({ query }) =>
		Response.json(
			Array.isArray(deliveries) ? { items: deliveries, next_cursor: null } : deliveries(query)
		)
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

const type = (label: string, value: string, scope: HTMLElement = document.body) =>
	fireEvent.input(within(scope).getByLabelText(label), { target: { value } });

// A destination URL is a credential the panel takes once and never shows again, and the
// address policy is stated where the URL is typed.
describe('the shared channels', () => {
	it('is closed without panel.settings', async () => {
		await open([actions.create]);
		expect(screen.getByText('Notification settings are not available to you.')).toBeTruthy();
		expect(screen.queryByLabelText('URL')).toBeNull();
	});

	it('says the URL is never shown again and which addresses are refused', async () => {
		await open([actions.panelSettings]);
		await screen.findAllByText('Ops channel');
		const page = text(document.body);
		expect(page).toContain('stores it encrypted and never shows it again');
		expect(page).toContain(
			'Addresses on this machine or its network are refused, and the panel does not follow redirects.'
		);
	});

	it('lists only the shared channels, not a server’s own', async () => {
		await open(
			[actions.panelSettings],
			[],
			[],
			[ops, webhook({ id: 'wh-2', name: 'Midgard only', instance_id: 'inst-a' })]
		);
		await screen.findAllByText('Ops channel');
		expect(screen.queryByTestId('channel-wh-2')).toBeNull();
		expect(text(document.body)).toContain('Midgard');
		expect(text(document.body)).toContain('1 own channel');
	});

	it('adds a channel and never quotes a URL back', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/webhooks', () => Response.json(ops, { status: 201 }));
		await screen.findAllByText('Ops channel');

		await type('Name', ' Raid alerts ');
		await type('URL', 'https://discord.com/api/webhooks/1/secret-token');
		await click(screen.getByRole('button', { name: 'Add channel' }));

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

		await type('Name', 'LAN');
		await type('URL', 'https://10.0.0.5/hook');
		await click(screen.getByRole('button', { name: 'Add channel' }));
		expect(await screen.findByText(/That address is on a private network\./)).toBeTruthy();
	});

	it('turns the built-in alerts off for one channel', async () => {
		await open([actions.panelSettings]);
		daemon.on('PATCH', '/admin/webhooks/wh-1', () => Response.json(ops));
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('switch', { name: 'Also post built-in alerts' }));
		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/webhooks/wh-1')[0]?.body).toEqual({
				catch_all: false
			})
		);
	});

	it('deletes a channel only after its name is typed back', async () => {
		await open([actions.panelSettings]);
		daemon.on('DELETE', '/admin/webhooks/wh-1', () => new Response(null, { status: 204 }));
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('button', { name: 'Delete Ops channel' }));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('the URL cannot be recovered');
		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'Ops channel' }
		});
		await click(within(dialog).getByRole('button', { name: 'Delete channel' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/admin/webhooks/wh-1')).toHaveLength(1)
		);
	});
});

/** The quiet fields a rule without quiet hours sends: an empty timezone clears any window. */
const noQuiet = { quiet_start_minutes: 0, quiet_end_minutes: 0, quiet_timezone: '' };

describe('the rules for every server', () => {
	it('lists the panel-wide rules with where they post and their settings', async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule({
					params: { crash_count: 5, template: '{stops} stops' },
					quiet_start_minutes: 1320,
					quiet_end_minutes: 420,
					quiet_timezone: 'Europe/Berlin'
				}),
				rule({ id: 'rule-2', instance_id: 'inst-a', condition_kind: 'player_joined' })
			]
		);
		const row = text(await screen.findByTestId('rule-rule-1'));
		expect(row).toContain('Crash loop');
		expect(row).toContain('→ Ops channel');
		expect(row).toContain('5 stops in 30 min');
		expect(row).toContain('quiet 22:00–07:00 Europe/Berlin');
		expect(row).toContain('{stops} stops');
		expect(screen.queryByTestId('rule-rule-2'), 'a server’s rule is on its own tab').toBeNull();
		expect(text(document.body)).toContain('1 rule');
	});

	it('adds a rule: picks the event, then posts it with the only channel ticked', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('button', { name: 'Add rule' }));
		const dialog = await screen.findByRole('dialog');
		expect(
			within(dialog).getByTestId('pick-low_disk'),
			'host-wide events are offered'
		).toBeTruthy();
		await click(within(dialog).getByTestId('pick-crash_loop'));
		await type('Stops', '4', dialog);
		await click(within(dialog).getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'crash_loop',
			webhook_ids: ['wh-1'],
			params: { crash_count: 4 },
			...noQuiet
		});
	});

	it('will not add a rule that posts nowhere', async () => {
		await open([actions.panelSettings], [], [], [ops, webhook({ id: 'wh-2', name: 'Friends' })]);
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('button', { name: 'Add rule' }));
		const dialog = await screen.findByRole('dialog');
		await click(within(dialog).getByTestId('pick-server_started'));
		expect(text(dialog)).toContain('Choose at least one channel to post to.');
		const add = within(dialog).getByRole('button', { name: 'Add rule' }) as HTMLButtonElement;
		expect(add.disabled).toBe(true);
	});

	it('edits a rule in place with its new message', async () => {
		await open([actions.panelSettings], [], [rule()]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule()));
		const row = await screen.findByTestId('rule-rule-1');

		await click(within(row).getByRole('button', { name: 'Edit the Crash loop rule' }));
		const dialog = await screen.findByRole('dialog');
		expect(within(dialog).queryByRole('button', { name: /Back/ }), 'the event is fixed').toBeNull();
		await type('Message', 'It keeps crashing: {stops}', dialog);
		await click(within(dialog).getByRole('button', { name: 'Save rule' }));

		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toMatchObject({
			condition_kind: 'crash_loop',
			params: { template: 'It keeps crashing: {stops}' }
		});
		expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(0);
	});

	it('duplicates a rule as a new one with its settings', async () => {
		const original = rule({ params: { template: 'Down again' } });
		await open([actions.panelSettings], [], [original]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(original, { status: 201 }));
		const row = await screen.findByTestId('rule-rule-1');

		await click(within(row).getByRole('button', { name: 'Duplicate the Crash loop rule' }));
		const dialog = await screen.findByRole('dialog');
		expect((within(dialog).getByLabelText('Message') as HTMLTextAreaElement).value).toBe(
			'Down again'
		);
		await click(within(dialog).getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toMatchObject({
			condition_kind: 'crash_loop',
			params: { template: 'Down again' }
		});
	});

	it('sends a test of the rule to each chosen channel, over the preview’s values', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules/test', () =>
			Response.json({ job_id: 'job-1', status: 'queued' }, { status: 202 })
		);
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('button', { name: 'Add rule' }));
		const dialog = await screen.findByRole('dialog');
		await click(within(dialog).getByTestId('pick-crash_loop'));
		await click(within(dialog).getByRole('button', { name: 'Send test' }));

		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/admin/alert-rules/test')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/admin/alert-rules/test')[0].body).toMatchObject({
			condition_kind: 'crash_loop',
			webhook_id: 'wh-1',
			fields: { stops: '3 in the last 30 minutes' }
		});
		expect(daemon.requests('POST', '/admin/alert-rules'), 'a test saves nothing').toHaveLength(0);
	});

	it('pauses a rule from its switch', async () => {
		await open([actions.panelSettings], [], [rule()]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule({ enabled: false })));
		const row = await screen.findByTestId('rule-rule-1');

		await click(within(row).getByRole('switch', { name: 'Send Crash loop' }));
		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0]?.body).toEqual({
				enabled: false
			})
		);
	});

	it('deletes a rule once confirmed', async () => {
		await open([actions.panelSettings], [], [rule()]);
		daemon.on('DELETE', '/admin/alert-rules/rule-1', () => new Response(null, { status: 204 }));
		const row = await screen.findByTestId('rule-rule-1');

		await click(within(row).getByRole('button', { name: 'Delete the Crash loop rule' }));
		const dialog = await screen.findByRole('dialog');
		await click(within(dialog).getByRole('button', { name: 'Delete rule' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
	});
});

const lastDeliveryQuery = () =>
	daemon.requests('GET', '/admin/webhooks/deliveries').at(-1)?.query ?? new URLSearchParams();

describe('the deliveries', () => {
	it('shows a failed delivery with its channel, server, reason and times', async () => {
		await open(
			[actions.panelSettings],
			[
				delivery({
					instance_id: 'inst-a',
					status: 'failed',
					attempts: 3,
					last_error: 'The destination answered 404.',
					updated_at: '2026-09-24T10:02:00Z'
				})
			]
		);

		expect(await screen.findByText('The destination answered 404.')).toBeTruthy();
		const row = text(screen.getByTestId('delivery-d-1'));
		expect(row).toContain('Backup failed → Ops channel');
		expect(row).toContain('Midgard');
		expect(row).toContain('Failed');
		expect(row).toContain('3 attempts');
		expect(row).toContain(`created ${new Date('2026-09-24T10:00:00Z').toLocaleString()}`);
		expect(row).toContain(`updated ${new Date('2026-09-24T10:02:00Z').toLocaleString()}`);
	});

	it('shows no error or update time on a delivered row that never changed', async () => {
		await open([actions.panelSettings], [delivery({ last_error: 'stale' })]);
		const row = await screen.findByTestId('delivery-d-1');
		expect(text(row)).not.toContain('stale');
		expect(text(row), 'an unchanged row shows one time').not.toContain('updated');
	});

	it.each([
		['test', 'Test notification'],
		['instance_down', 'Server stopped unexpectedly'],
		['update_available', 'Server update available'],
		['backup_failed', 'Backup failed'],
		['alert_opened', 'Alert raised'],
		['alert_resolved', 'Alert cleared'],
		['power_cut_soon', 'Power cut soon'],
		['server_started', 'Server started'],
		['player_joined', 'Player joined'],
		['some_new_kind', 'some new kind']
	])('names a %s delivery as “%s”', async (kind, label) => {
		await open([actions.panelSettings], [delivery({ event_kind: kind })]);
		const row = await screen.findByTestId('delivery-d-1');
		expect(text(row)).toContain(`${label} → Ops channel`);
	});

	it('asks the daemon for one channel and one status', async () => {
		await open([actions.panelSettings], [delivery()]);
		await screen.findByTestId('delivery-d-1');
		expect(lastDeliveryQuery().toString()).toBe('');

		await choose(screen.getByLabelText('Channel'), 'Ops channel');
		await vi.waitFor(() => expect(lastDeliveryQuery().get('webhook_id')).toBe('wh-1'));
		await choose(screen.getByLabelText('Status'), 'Failed');
		await vi.waitFor(() => expect(lastDeliveryQuery().get('status')).toBe('failed'));
		expect(lastDeliveryQuery().get('webhook_id')).toBe('wh-1');

		await choose(screen.getByLabelText('Status'), 'Any status');
		await vi.waitFor(() => expect(lastDeliveryQuery().has('status')).toBe(false));
	});

	it('says when nothing matches the filters', async () => {
		await open([actions.panelSettings], (query) => ({
			items: query.get('status') === 'pending' ? [] : [delivery()],
			next_cursor: null
		}));
		await screen.findByTestId('delivery-d-1');
		await choose(screen.getByLabelText('Status'), 'Pending');
		expect(await screen.findByText('No deliveries match these filters.')).toBeTruthy();
	});

	it('clears old rows when a filtered request fails', async () => {
		await open([actions.panelSettings], [delivery()]);
		await screen.findByTestId('delivery-d-1');
		daemon.on('GET', '/admin/webhooks/deliveries', () =>
			envelope(503, 'unavailable', 'Could not load deliveries.')
		);

		await choose(screen.getByLabelText('Status'), 'Pending');
		expect(await screen.findByText('Could not load deliveries.')).toBeTruthy();
		expect(screen.queryByTestId('delivery-d-1')).toBeNull();
		expect(screen.queryByText('No deliveries match these filters.')).toBeNull();
		expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
	});

	it('loads more with the same filters and the cursor', async () => {
		await open([actions.panelSettings], (query) =>
			query.get('cursor') === 'c-1'
				? { items: [delivery({ id: 'd-2', status: 'failed' })], next_cursor: null }
				: { items: [delivery({ status: 'failed' })], next_cursor: 'c-1' }
		);
		await screen.findByTestId('delivery-d-1');
		await choose(screen.getByLabelText('Status'), 'Failed');
		await vi.waitFor(() => expect(lastDeliveryQuery().get('status')).toBe('failed'));

		await click(await screen.findByRole('button', { name: 'Load more' }));
		expect(await screen.findByTestId('delivery-d-2')).toBeTruthy();
		expect(screen.getByTestId('delivery-d-1'), 'the first page stays').toBeTruthy();
		expect(lastDeliveryQuery().get('cursor')).toBe('c-1');
		expect(lastDeliveryQuery().get('status')).toBe('failed');
		expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
	});

	it('shows only the deliveries a rule sent, and clears back to all', async () => {
		await open([actions.panelSettings], [delivery()], [rule()]);
		const row = await screen.findByTestId('rule-rule-1');

		await click(within(row).getByRole('button', { name: 'Deliveries' }));
		await vi.waitFor(() => expect(lastDeliveryQuery().get('rule_id')).toBe('rule-1'));
		const banner = screen.getByTestId('delivery-rule-filter');
		expect(text(banner)).toContain('Only messages sent by the Crash loop rule.');

		await click(within(banner).getByRole('button', { name: 'Clear rule filter' }));
		await vi.waitFor(() => expect(lastDeliveryQuery().has('rule_id')).toBe(false));
		expect(screen.queryByTestId('delivery-rule-filter')).toBeNull();
	});
});
