import { render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Invite } from '$lib/api/admin';
import type { GrantPage } from '$lib/api/grants';
import type { User } from '$lib/api/types';
import { FakeDaemon, instance } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({
	page: { url: new URL('http://localhost/admin/invites') }
}));

vi.mock('$app/state', () => app);

const hour = 3_600_000;
const day = 24 * hour;
const at = (offset: number) => new Date(Date.now() + offset).toISOString();
const shown = (iso: string) => new Date(iso).toLocaleString();

function user(id: string, username: string): User {
	return {
		id,
		username,
		role: 'member',
		disabled: false,
		owner: false,
		created_at: '2026-09-01T00:00:00Z',
		last_login_at: null
	};
}

function invite(id: string, overrides: Partial<Invite> = {}): Invite {
	return {
		id,
		created_by: 'u-root',
		instance_id: null,
		grant_role: null,
		grant_perms: [],
		expires_at: at(3 * day + hour),
		created_at: at(-day),
		redeemed_at: null,
		redeemed_by: null,
		revoked_at: null,
		...overrides
	};
}

let daemon: FakeDaemon;
let invites: Invite[];

/** Serves two servers, Alpha and Beta, and five invites: two active (one expiring within the
 * hour), one redeemed by alice, one revoked and one expired. */
function serve() {
	invites = [
		invite('i-far', { instance_id: 'inst-a', grant_role: 'viewer' }),
		invite('i-soon', { expires_at: at(2 * hour) }),
		invite('i-used', { redeemed_at: at(-hour), redeemed_by: 'u-a' }),
		invite('i-revoked', { revoked_at: at(-2 * hour) }),
		invite('i-old', { expires_at: at(-day) })
	];
	daemon.on('GET', '/invites', () => Response.json({ items: invites, next_cursor: null }));
	daemon.on('GET', '/users', () =>
		Response.json({ items: [{ ...user('u-root', 'root'), role: 'admin' }, user('u-a', 'alice')] })
	);
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: [instance({ id: 'inst-a', name: 'Alpha' }), instance({ id: 'inst-b', name: 'Beta' })],
			next_cursor: null
		})
	);
	daemon.on('GET', '/instances/inst-b/grants', () =>
		Response.json({
			items: [],
			next_cursor: null,
			roles: [{ role: 'viewer', allowed_actions: [] }],
			extra_capabilities: []
		} satisfies GrantPage)
	);
	daemon.on('POST', '/invites', () =>
		Response.json(
			{ token: 'tok', url: 'http://panel/redeem/tok', expires_at: at(7 * day) },
			{ status: 201 }
		)
	);
}

async function open(search = '') {
	app.page.url = new URL(`http://localhost/admin/invites${search}`);
	render(Page);
	await screen.findByRole('group', { name: 'Invite status' });
}

/** The invite cards the history currently lists. */
function rows(): HTMLElement[] {
	const section = screen.getByRole('heading', { name: 'Invite history' }).closest('section')!;
	return [...section.querySelectorAll<HTMLElement>('[data-slot="card"]')];
}

const pick = (name: RegExp) => click(screen.getByRole('button', { name }));

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	serve();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('invite history filters', () => {
	it('lists only active invites by default and counts every status', async () => {
		await open();

		const filters = within(screen.getByRole('group', { name: 'Invite status' }))
			.getAllByRole('button')
			.map((button) => [text(button).trim(), button.getAttribute('aria-pressed')]);
		expect(filters).toEqual([
			['Active 2', 'true'],
			['Redeemed 1', 'false'],
			['Revoked 1', 'false'],
			['Expired 1', 'false'],
			['All 5', 'false']
		]);
		expect(rows()).toHaveLength(2);
		expect(rows().every((row) => within(row).queryByRole('button', { name: /revoke/i }))).toBe(
			true
		);
	});

	it('shows who redeemed an invite and when, with nothing to revoke', async () => {
		await open();
		await pick(/^Redeemed/);

		const [row] = rows();
		expect(rows()).toHaveLength(1);
		expect(text(row)).toContain(`Redeemed by alice on ${shown(invites[2].redeemed_at!)}`);
		expect(within(row).queryByRole('button', { name: /revoke/i })).toBeNull();
	});

	it('shows when an invite was revoked', async () => {
		await open();
		await pick(/^Revoked/);

		expect(rows()).toHaveLength(1);
		expect(text(rows()[0])).toContain(`Revoked on ${shown(invites[3].revoked_at!)}`);
	});

	it('shows when an invite expired', async () => {
		await open();
		await pick(/^Expired/);

		expect(rows()).toHaveLength(1);
		expect(text(rows()[0])).toContain(`Expired on ${shown(invites[4].expires_at)}`);
	});

	it('lists every invite under All', async () => {
		await open();
		await pick(/^All/);
		expect(rows()).toHaveLength(5);
	});

	it('says so when a status has no invites', async () => {
		invites.splice(3, 1);
		await open();
		await pick(/^Revoked/);

		expect(rows()).toHaveLength(0);
		expect(screen.getByText('No revoked invites.')).toBeTruthy();
	});
});

describe('active invite expiry', () => {
	it('shows the relative and absolute expiry, and warns within a day', async () => {
		await open();
		const [far, soon] = rows();

		const farLine = within(far).getByText(/^Expires/);
		expect(text(farLine).trim()).toBe(`Expires in 3 days (${shown(invites[0].expires_at)})`);
		expect(farLine.className).not.toContain('amber');

		const soonLine = within(soon).getByText(/^Expires/);
		expect(text(soonLine).trim()).toMatch(/^Expires in (1|2) hours? \(/);
		expect(soonLine.className).toContain('text-amber-800');
	});
});

describe('the ?instance= link', () => {
	const selected = () => text(document.querySelector('#invite-instance')).trim();

	it('preselects that server and issues the invite for it', async () => {
		await open('?instance=inst-b');
		await vi.waitFor(() => expect(selected()).toBe('Beta'));
		await vi.waitFor(() =>
			expect(screen.getByRole('button', { name: 'Create invite' })).toHaveProperty(
				'disabled',
				false
			)
		);

		await click(screen.getByRole('button', { name: 'Create invite' }));
		await screen.findByText('Copy this invite now');
		expect(daemon.requests('POST', '/invites')[0].body).toEqual({
			instance_id: 'inst-b',
			grant_role: 'viewer',
			grant_perms: []
		});
	});

	it('ignores a server the caller cannot see', async () => {
		await open('?instance=inst-missing');
		expect(selected()).toBe('No server yet');
	});
});
