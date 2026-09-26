import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Delivery, Webhook } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, permissions } from '$lib/testing/daemon';
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

async function open(global: string[], deliveries: Delivery[] = []) {
	daemon.on('GET', '/admin/webhooks', () => Response.json({ items: [ops], next_cursor: null }));
	daemon.on('GET', '/admin/webhooks/deliveries', () =>
		Response.json({ items: deliveries, next_cursor: null })
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
		await screen.findByText('Ops channel');
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
		await screen.findByText('Ops channel');

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
		await screen.findByText('Ops channel');

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
		await screen.findByText('Ops channel');

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
