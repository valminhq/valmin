import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AlertRule, Delivery, DeliveryPage, Webhook } from '$lib/api/admin';
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
	destinations: Webhook[] = [ops]
) {
	daemon.on('GET', '/admin/webhooks', () =>
		Response.json({ items: destinations, next_cursor: null })
	);
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
		expect(page).toContain('posted to every channel unless an event below already covers them');
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

	it('shows a failed delivery with its destination, server, reason and times', async () => {
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
		['instance_auto_stopped', 'Server stopped: no players'],
		['power_cut_soon', 'Power cut soon'],
		['server_started', 'Server started'],
		['server_stopped', 'Server stopped'],
		['some_new_kind', 'some new kind']
	])('names a %s delivery as “%s”', async (kind, label) => {
		await open([actions.panelSettings], [delivery({ event_kind: kind })]);
		const row = await screen.findByTestId('delivery-d-1');
		expect(text(row)).toContain(`${label} → Ops channel`);
	});
});

const lastDeliveryQuery = () =>
	daemon.requests('GET', '/admin/webhooks/deliveries').at(-1)?.query ?? new URLSearchParams();

describe('the delivery filters', () => {
	it('asks the daemon for one destination and one status', async () => {
		await open([actions.panelSettings], [delivery()]);
		await screen.findByTestId('delivery-d-1');
		expect(lastDeliveryQuery().toString()).toBe('');

		await choose(screen.getByLabelText('Destination'), 'Ops channel');
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
		const row = await screen.findByTestId('event-crash_loop');

		await click(within(row).getByRole('button', { name: 'Deliveries' }));
		await vi.waitFor(() => expect(lastDeliveryQuery().get('rule_id')).toBe('rule-1'));
		const banner = screen.getByTestId('delivery-rule-filter');
		expect(text(banner)).toContain('Only messages sent for Crash loop on every server.');
		expect(text(banner)).toContain('not listed');

		await click(within(banner).getByRole('button', { name: 'Clear rule filter' }));
		await vi.waitFor(() => expect(lastDeliveryQuery().has('rule_id')).toBe(false));
		expect(screen.queryByTestId('delivery-rule-filter')).toBeNull();
	});
});

/** The quiet fields a rule without quiet hours sends: an empty timezone clears any window. */
const noQuiet = { quiet_start_minutes: 0, quiet_end_minutes: 0, quiet_timezone: '' };
const berlin = {
	quiet_start_minutes: 1320,
	quiet_end_minutes: 420,
	quiet_timezone: 'Europe/Berlin'
};

const type = (label: string, value: string, within_: HTMLElement = document.body) =>
	fireEvent.input(within(within_).getByLabelText(label), { target: { value } });
const save = () => click(screen.getByRole('button', { name: 'Save changes' }));
const saveButton = () => screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement;
const row = (kind: string) => screen.getByTestId(`event-${kind}`);
const checked = (el: HTMLElement, name: string) =>
	(within(el).getByRole('checkbox', { name }) as HTMLInputElement).checked;

describe('the events', () => {
	it('switches an event on with the default channel and posts its rule', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await click(await screen.findByRole('switch', { name: 'Crash loop' }));
		await save();

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'crash_loop',
			instance_id: null,
			webhook_ids: ['wh-1'],
			params: {},
			...noQuiet
		});
	});

	it('takes the defaults most rules share, and shows an event that differs as its own', async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule({ id: 'rule-1', condition_kind: 'crash_loop', ...berlin }),
				rule({ id: 'rule-2', condition_kind: 'job_failed', ...berlin }),
				rule({ id: 'rule-3', condition_kind: 'job_stuck' })
			]
		);
		await screen.findByTestId('event-job_stuck');
		const defaults = screen.getByTestId('defaults');
		expect((within(defaults).getByLabelText('From') as HTMLInputElement).value).toBe('22:00');
		expect(checked(row('crash_loop'), 'Use the default quiet hours')).toBe(true);
		expect(checked(row('job_stuck'), 'Use the default quiet hours')).toBe(false);
		expect(
			screen.queryByRole('button', { name: 'Save changes' }),
			'nothing is unsaved on load'
		).toBeNull();
	});

	it('patches only the rules whose settings changed, with every field', async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule({ id: 'rule-1', condition_kind: 'job_stuck' }),
				rule({ id: 'rule-2', condition_kind: 'job_failed' })
			]
		);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule()));
		await screen.findByTestId('event-job_stuck');

		await type('Minutes a job may run', '15');
		await save();

		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toEqual({
			condition_kind: 'job_stuck',
			instance_id: '',
			webhook_ids: ['wh-1'],
			params: { stuck_after_seconds: 900 },
			...noQuiet,
			enabled: true
		});
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-2')).toHaveLength(0);
	});

	it('turns an event off by pausing its rule', async () => {
		await open([actions.panelSettings], [], [rule()]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule({ enabled: false })));
		await click(await screen.findByRole('switch', { name: 'Crash loop' }));
		await save();

		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toMatchObject({
			enabled: false
		});
		expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(0);
	});

	it('gives every event that follows them the new default quiet hours', async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule({ id: 'rule-1', condition_kind: 'crash_loop' }),
				rule({ id: 'rule-2', condition_kind: 'job_failed' })
			]
		);
		for (const id of ['rule-1', 'rule-2']) {
			daemon.on('PATCH', `/admin/alert-rules/${id}`, () => Response.json(rule({ id })));
		}
		await screen.findByTestId('event-job_failed');
		const defaults = screen.getByTestId('defaults');

		await click(within(defaults).getByRole('switch', { name: 'Quiet hours' }));
		await type('From', '23:30', defaults);
		await type('Until', '06:15', defaults);
		await type('Timezone', 'Europe/Kyiv', defaults);
		await save();

		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-2')).toHaveLength(1)
		);
		for (const id of ['rule-1', 'rule-2']) {
			expect(daemon.requests('PATCH', `/admin/alert-rules/${id}`)[0].body).toMatchObject({
				quiet_start_minutes: 1410,
				quiet_end_minutes: 375,
				quiet_timezone: 'Europe/Kyiv'
			});
		}
	});

	it('posts an event to its own channels when it does not use the default', async () => {
		const players: Webhook = { ...ops, id: 'wh-2', name: 'Players' };
		await open([actions.panelSettings], [], [], [ops, players]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findByTestId('defaults');
		await click(
			within(screen.getByTestId('defaults')).getByRole('checkbox', { name: 'Ops channel' })
		);

		await click(screen.getByRole('switch', { name: 'Server started' }));
		await click(
			within(row('server_started')).getByRole('checkbox', { name: 'Post to the default channels' })
		);
		await click(within(row('server_started')).getByRole('checkbox', { name: 'Ops channel' }));
		await click(within(row('server_started')).getByRole('checkbox', { name: 'Players' }));
		await type('Timezone for times in the message', 'Europe/Berlin');
		await save();

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toMatchObject({
			condition_kind: 'server_started',
			webhook_ids: ['wh-2'],
			params: { timezone: 'Europe/Berlin' }
		});
	});

	it('will not save an event with no channel, a bad number or an empty quiet window', async () => {
		const players: Webhook = { ...ops, id: 'wh-2', name: 'Players' };
		await open([actions.panelSettings], [], [], [ops, players]);
		await click(await screen.findByRole('switch', { name: 'Crash loop' }));
		expect(text(document.body)).toContain('Choose at least one channel for Crash loop.');
		expect(saveButton().disabled).toBe(true);

		const defaults = screen.getByTestId('defaults');
		await click(within(defaults).getByRole('checkbox', { name: 'Ops channel' }));
		expect(saveButton().disabled).toBe(false);

		await type('Stops', '1.5');
		expect(text(document.body)).toContain('Check the numbers for Crash loop.');
		await type('Stops', '4');

		await click(within(defaults).getByRole('switch', { name: 'Quiet hours' }));
		await type('From', '00:00', defaults);
		await type('Until', '00:00', defaults);
		expect(saveButton().disabled).toBe(true);
		await type('Until', '06:00', defaults);
		expect(saveButton().disabled).toBe(false);
	});

	it("sets one server's events, without the host-wide ones, and marks those on for every server", async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule({ id: 'rule-1', condition_kind: 'crash_loop' }),
				rule({ id: 'rule-2', condition_kind: 'job_failed', instance_id: 'inst-a' })
			]
		);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findByTestId('event-low_disk');

		await choose(screen.getByLabelText('Settings for'), 'Midgard');
		expect(screen.queryByTestId('event-low_disk')).toBeNull();
		expect(screen.queryByTestId('event-power_cut')).toBeNull();
		expect(text(row('crash_loop'))).toContain('On for every server');
		expect(
			(screen.getByRole('switch', { name: 'Job failed' }) as HTMLElement).getAttribute(
				'aria-checked'
			)
		).toBe('true');

		await click(screen.getByRole('switch', { name: 'Server stopped' }));
		expect(
			(screen.getByLabelText('Settings for') as HTMLButtonElement).disabled,
			'the scope holds while changes are unsaved'
		).toBe(true);
		await save();

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toMatchObject({
			condition_kind: 'server_stopped',
			instance_id: 'inst-a'
		});
	});

	it('turns hidden fields into a template without their lines', async () => {
		await open(
			[actions.panelSettings],
			[],
			[rule({ condition_kind: 'server_started', params: { hidden_fields: ['world', 'port'] } })]
		);
		const started = await screen.findByTestId('event-server_started');
		const message = within(started).getByLabelText('Message') as HTMLTextAreaElement;
		expect(message.value).toBe(
			'**Server name:** {server_name}\n**Join code:** {join_code}\n**Started:** {time}'
		);
		const preview = text(within(started).getByTestId('preview-server_started'));
		expect(preview).toContain('Midgard · Server started');
		expect(preview).not.toContain('Dedicated');
	});

	it('previews a host-wide event without a server, in the chosen style', async () => {
		await open(
			[actions.panelSettings],
			[],
			[rule({ condition_kind: 'low_disk', params: { layout: 'plain' } })]
		);
		const disk = await screen.findByTestId('event-low_disk');
		const preview = within(disk).getByTestId('preview-low_disk');
		expect(text(preview)).toContain('Free space: 4.5 GB');
		expect(text(preview)).not.toContain('The host is running low on disk space');
		expect(text(disk)).toContain('this message is edited to show it’s resolved');
	});
});

describe('game event rules', () => {
	it('lists only every-server game event rules, for removal', async () => {
		await open(
			[actions.panelSettings],
			[],
			[
				rule({ id: 'rule-join', condition_kind: 'player_joined', instance_id: 'inst-a' }),
				rule({ id: 'rule-raid', condition_kind: 'raid_started', instance_id: null })
			]
		);
		const raid = await screen.findByTestId('game-rule-rule-raid');
		expect(text(raid)).toContain('Raid started → Ops channel');
		expect(screen.queryByTestId('game-rule-rule-join')).toBeNull();
		expect(screen.queryByRole('switch', { name: 'Player joined' })).toBeNull();
	});
});
