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

/** Swaps the new rule form's default Crash loop tick for the named conditions. */
async function only(...conditions: string[]) {
	await click(screen.getByRole('checkbox', { name: 'Crash loop' }));
	for (const c of conditions) await click(screen.getByRole('checkbox', { name: c }));
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
		await open([actions.panelSettings], [delivery()], [rule({ instance_id: 'inst-a' })]);
		const row = await screen.findByTestId('alert-rule-rule-1');

		await click(within(row).getByRole('button', { name: 'Show deliveries' }));
		await vi.waitFor(() => expect(lastDeliveryQuery().get('rule_id')).toBe('rule-1'));
		const banner = screen.getByTestId('delivery-rule-filter');
		expect(text(banner)).toContain('Only alerts sent by the Crash loop rule for Midgard.');
		expect(text(banner)).toContain('not listed');

		await click(within(banner).getByRole('button', { name: 'Clear rule filter' }));
		await vi.waitFor(() => expect(lastDeliveryQuery().has('rule_id')).toBe(false));
		expect(screen.queryByTestId('delivery-rule-filter')).toBeNull();
	});
});

/** The quiet fields a rule without quiet hours sends: an empty timezone clears any window. */
const noQuiet = { quiet_start_minutes: 0, quiet_end_minutes: 0, quiet_timezone: '' };

const type = (label: string, value: string) =>
	fireEvent.input(screen.getByLabelText(label), { target: { value } });

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
		expect(text(first), 'defaults are spelled out').toContain('3 stops in 30 min');

		const second = screen.getByTestId('alert-rule-rule-2');
		expect(text(second)).toContain('Backups stale');
		expect(text(second)).toContain('Midgard');
		expect(text(second)).toContain('3× the backup interval');
		expect(text(second)).toContain('Quiet 22:00–07:00, Europe/Berlin');
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

		await only('Job stuck');
		await choose(screen.getByLabelText('Server'), 'Midgard');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		expect(add.disabled).toBe(false);
		await click(add);

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'job_stuck',
			instance_id: 'inst-a',
			webhook_ids: ['wh-1'],
			params: {},
			...noQuiet
		});
	});

	it.each([
		['Power cut soon', 'power_cut', null],
		['Server stopped', 'server_stopped', 'inst-a']
	])('posts a %s rule with its scope', async (label, kind, instanceID) => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		await choose(screen.getByLabelText('Server'), 'Midgard');
		await only(label);
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toMatchObject({
			condition_kind: kind,
			instance_id: instanceID
		});
	});

	it('posts a server started rule with the fields it hides and its message timezone', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		await only('Server started');
		await click(screen.getByRole('switch', { name: 'World' }));
		await type('Timezone for times in the message', 'Europe/Berlin');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toMatchObject({
			condition_kind: 'server_started',
			params: { hidden_fields: ['world'], timezone: 'Europe/Berlin' }
		});
	});

	it('posts a low disk rule for every server, since low disk is host-wide', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		await choose(screen.getByLabelText('Server'), 'Midgard');
		await only('Low disk');
		const server = screen.getByLabelText('Server') as HTMLButtonElement;
		expect(server.disabled).toBe(true);
		expect(text(server)).toContain('Every server');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toEqual({
			condition_kind: 'low_disk',
			instance_id: null,
			webhook_ids: ['wh-1'],
			params: {},
			...noQuiet
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

		await click(within(row).getByRole('button', { name: 'Delete the Crash loop rule' }));
		const dialog = await screen.findByRole('dialog');
		expect(daemon.requests('DELETE', '/admin/alert-rules/rule-1')).toHaveLength(0);
		await click(within(dialog).getByRole('button', { name: 'Delete rule' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
	});

	it('loads a rule into the form and saves every field with a PATCH', async () => {
		const stuck = rule({
			condition_kind: 'job_stuck',
			instance_id: 'inst-a',
			params: { stuck_after_seconds: 5400 },
			quiet_start_minutes: 1320,
			quiet_end_minutes: 420,
			quiet_timezone: 'Europe/Berlin'
		});
		await open([actions.panelSettings], [], [stuck]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(stuck));
		const row = await screen.findByTestId('alert-rule-rule-1');
		expect(text(row)).toContain('after 90 min');

		await click(within(row).getByRole('button', { name: /^Edit the .* rule$/ }));
		expect(text(screen.getByLabelText('Condition'))).toContain('Job stuck');
		expect(text(screen.getByLabelText('Server'))).toContain('Midgard');
		expect((screen.getByLabelText('Minutes a job may run') as HTMLInputElement).value).toBe('90');
		expect((screen.getByLabelText('From') as HTMLInputElement).value).toBe('22:00');
		expect((screen.getByLabelText('Timezone') as HTMLInputElement).value).toBe('Europe/Berlin');
		expect(screen.queryByRole('button', { name: 'Add rule' })).toBeNull();

		await type('Minutes a job may run', '45');
		await click(screen.getByRole('button', { name: 'Save rule' }));
		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toEqual({
			condition_kind: 'job_stuck',
			instance_id: 'inst-a',
			webhook_ids: ['wh-1'],
			params: { stuck_after_seconds: 2700 },
			quiet_start_minutes: 1320,
			quiet_end_minutes: 420,
			quiet_timezone: 'Europe/Berlin'
		});
		expect(await screen.findByRole('button', { name: 'Add rule' }), 'the form resets').toBeTruthy();
	});

	it('cancels an edit back to an empty form', async () => {
		await open([actions.panelSettings], [], [rule({ condition_kind: 'job_stuck' })]);
		const row = await screen.findByTestId('alert-rule-rule-1');

		await click(within(row).getByRole('button', { name: /^Edit the .* rule$/ }));
		await click(screen.getByRole('button', { name: 'Cancel' }));
		const checked = (name: string) =>
			(screen.getByRole('checkbox', { name }) as HTMLInputElement).checked;
		expect(checked('Crash loop')).toBe(true);
		expect(checked('Job stuck')).toBe(false);
		expect(
			(screen.getByRole('checkbox', { name: 'Ops channel' }) as HTMLInputElement).checked
		).toBe(false);
		expect(screen.getByRole('button', { name: 'Add rule' })).toBeTruthy();
	});

	it('clears quiet hours when they are turned off on an existing rule', async () => {
		const quiet = rule({
			quiet_start_minutes: 1320,
			quiet_end_minutes: 420,
			quiet_timezone: 'Europe/Berlin'
		});
		await open([actions.panelSettings], [], [quiet]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule()));
		const row = await screen.findByTestId('alert-rule-rule-1');

		await click(within(row).getByRole('button', { name: /^Edit the .* rule$/ }));
		await click(screen.getByRole('switch', { name: 'Quiet hours' }));
		expect(screen.queryByLabelText('Timezone')).toBeNull();
		await click(screen.getByRole('button', { name: 'Save rule' }));
		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toMatchObject(noQuiet);
	});

	it('shows only the ticked kinds’ thresholds and sends durations in seconds', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		expect(screen.getByLabelText('Stops').getAttribute('placeholder')).toBe('3');
		expect(screen.getByLabelText('Within minutes').getAttribute('placeholder')).toBe('30');
		expect(screen.queryByLabelText('Minutes a job may run')).toBeNull();
		await type('Stops', '5');
		await type('Within minutes', '10');

		await only('Backups stale');
		expect(screen.queryByLabelText('Stops')).toBeNull();
		expect(screen.getByLabelText('Times the backup interval').getAttribute('placeholder')).toBe(
			'2'
		);
		await type('Times the backup interval', '3');
		await click(screen.getByRole('checkbox', { name: 'Backups stale' }));
		await click(screen.getByRole('checkbox', { name: 'Crash loop' }));
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		const body = daemon.requests('POST', '/admin/alert-rules')[0].body as AlertRule;
		expect(body.condition_kind).toBe('crash_loop');
		expect(body.params).toEqual({ crash_count: 5, crash_window_seconds: 600 });
	});

	it('blocks saving an out-of-range threshold', async () => {
		await open([actions.panelSettings]);
		await screen.findAllByText('Ops channel');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		const add = screen.getByRole('button', { name: 'Add rule' }) as HTMLButtonElement;

		await type('Stops', '-1');
		expect(add.disabled).toBe(true);
		expect(text(document.body)).toContain('Enter a whole number, 0 or more.');
		await type('Stops', '');
		expect(add.disabled).toBe(false);

		await only('Backups stale');
		await type('Times the backup interval', '1');
		expect(add.disabled).toBe(true);
		expect(text(document.body)).toContain('Enter a number above 1.');
		await type('Times the backup interval', '1.5');
		expect(add.disabled).toBe(false);
	});

	it('posts quiet hours as minutes from midnight with the timezone', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () => Response.json(rule(), { status: 201 }));
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('switch', { name: 'Quiet hours' }));
		expect(text(document.body)).toContain('Alerts still open when quiet hours end are sent then');
		await type('From', '23:30');
		await type('Until', '06:15');
		await type('Timezone', 'Europe/Kyiv');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(1));
		expect(daemon.requests('POST', '/admin/alert-rules')[0].body).toMatchObject({
			quiet_start_minutes: 1410,
			quiet_end_minutes: 375,
			quiet_timezone: 'Europe/Kyiv'
		});
	});

	it('blocks a quiet window whose start equals its end', async () => {
		await open([actions.panelSettings]);
		await screen.findAllByText('Ops channel');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		const add = screen.getByRole('button', { name: 'Add rule' }) as HTMLButtonElement;

		await click(screen.getByRole('switch', { name: 'Quiet hours' }));
		await type('From', '00:00');
		await type('Until', '00:00');
		expect(add.disabled).toBe(true);
		expect(text(document.body)).toContain('start and end must differ');
		await type('Until', '06:00');
		expect(add.disabled).toBe(false);
	});

	it.each([
		['Every server', true, 'Crash loop'],
		['Low disk', false, 'Low disk']
	])('sends an empty instance_id when a server rule is edited to %s', async (_, every, kind) => {
		await open([actions.panelSettings], [], [rule({ instance_id: 'inst-a' })]);
		daemon.on('PATCH', '/admin/alert-rules/rule-1', () => Response.json(rule()));
		const row = await screen.findByTestId('alert-rule-rule-1');

		await click(within(row).getByRole('button', { name: /^Edit the .* rule$/ }));
		if (every) await choose(screen.getByLabelText('Server'), 'Every server');
		await choose(screen.getByLabelText('Condition'), kind);
		expect(text(screen.getByLabelText('Server'))).toContain('Every server');
		await click(screen.getByRole('button', { name: 'Save rule' }));
		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', '/admin/alert-rules/rule-1')[0].body).toMatchObject({
			instance_id: ''
		});
	});

	it('posts one rule per ticked condition, each with its own thresholds and scope', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', ({ body }) => {
			const kind = (body as AlertRule).condition_kind;
			return Response.json(rule({ id: kind, condition_kind: kind }), { status: 201 });
		});
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('checkbox', { name: 'Job stuck' }));
		await click(screen.getByRole('checkbox', { name: 'Low disk' }));
		await type('Stops', '5');
		await type('Minutes a job may run', '45');
		await choose(screen.getByLabelText('Server'), 'Midgard');
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		await vi.waitFor(() => expect(daemon.requests('POST', '/admin/alert-rules')).toHaveLength(3));
		const bodies = daemon.requests('POST', '/admin/alert-rules').map((r) => r.body);
		expect(bodies).toEqual([
			expect.objectContaining({
				condition_kind: 'crash_loop',
				instance_id: 'inst-a',
				params: { crash_count: 5 }
			}),
			expect.objectContaining({
				condition_kind: 'job_stuck',
				instance_id: 'inst-a',
				params: { stuck_after_seconds: 2700 }
			}),
			expect.objectContaining({ condition_kind: 'low_disk', instance_id: null, params: {} })
		]);
	});

	it('unticks the conditions already saved when a later one is refused', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', ({ body }) =>
			(body as AlertRule).condition_kind === 'crash_loop'
				? Response.json(rule(), { status: 201 })
				: envelope(422, 'validation_failed', 'Refused.')
		);
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('checkbox', { name: 'Job stuck' }));
		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));

		expect(await screen.findByText(/Refused\./)).toBeTruthy();
		expect(screen.getByTestId('alert-rule-rule-1')).toBeTruthy();
		const checked = (name: string) =>
			(screen.getByRole('checkbox', { name }) as HTMLInputElement).checked;
		expect(checked('Crash loop')).toBe(false);
		expect(checked('Job stuck')).toBe(true);
	});

	it('shows the daemon’s refusal of a rule', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/alert-rules', () =>
			envelope(422, 'validation_failed', 'Not a timezone this host knows.')
		);
		await screen.findAllByText('Ops channel');

		await click(screen.getByRole('checkbox', { name: 'Ops channel' }));
		await click(screen.getByRole('button', { name: 'Add rule' }));
		expect(await screen.findByText(/Not a timezone this host knows\./)).toBeTruthy();
	});
});
