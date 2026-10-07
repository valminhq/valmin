import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Grant, GrantPage, GrantRole, ReplaceGrant } from '$lib/api/grants';
import { actions } from '$lib/api/instances';
import type { User } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/access') }
}));

vi.mock('$app/state', () => app);

const viewer = [
	'backups.list',
	'config.read',
	'console.read',
	'instance.view',
	'mods.list',
	'stats.read'
];
const operator = [
	...viewer,
	'backups.create',
	'backups.download',
	'commands.send',
	'instance.restart',
	'instance.start',
	'instance.stop',
	'players.manage'
];

function user(id: string, username: string, overrides: Partial<User> = {}): User {
	return {
		id,
		username,
		role: 'member',
		disabled: false,
		owner: false,
		created_at: '2026-09-01T00:00:00Z',
		last_login_at: null,
		timezone: '',
		...overrides
	};
}

function grant(userId: string, role: GrantRole, perms: string[] = [], etag = '"v1"'): Grant {
	return {
		user_id: userId,
		instance_id: 'inst-a',
		role,
		perms,
		granted_by: 'u-root',
		granted_at: '2026-09-01T00:00:00Z',
		expires_at: null,
		etag
	};
}

const catalogue = (items: Grant[]): GrantPage => ({
	items,
	next_cursor: null,
	roles: [
		{ role: 'viewer', allowed_actions: viewer },
		{ role: 'operator', allowed_actions: operator }
	],
	extra_capabilities: [
		{
			action: 'mods.manage',
			risk: 'Can install, update, and extract arbitrary third-party archives.'
		},
		{
			action: 'backups.restore',
			risk: 'Can replace the live world and permanently delete backup archives.'
		}
	]
});

const held = permissions('inst-a', [], [actions.grantsManage]);
const path = '/instances/inst-a/grants';

let daemon: FakeDaemon;

/** Serves the page: alice and bob hold grants, carol and dave do not, root is an administrator. */
function serve(users: User[] = []) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(instance({ name: 'Live' })));
	daemon.on('GET', path, () =>
		Response.json(catalogue([grant('u-a', 'viewer'), grant('u-b', 'operator')]))
	);
	daemon.on('GET', '/users', () =>
		Response.json({
			items: [
				user('u-root', 'root', { role: 'admin', owner: true }),
				user('u-a', 'alice'),
				user('u-b', 'bob'),
				user('u-c', 'carol'),
				user('u-d', 'dave'),
				...users
			]
		})
	);
	daemon.on('GET', '/me/permissions', () => Response.json(held));
	for (const id of ['u-a', 'u-b', 'u-c', 'u-d']) {
		daemon.on('PUT', `${path}/${id}`, (request) => {
			const body = request.body as ReplaceGrant;
			return Response.json(grant(id, body.role, body.perms, '"v2"'));
		});
		daemon.on('DELETE', `${path}/${id}`, () => new Response(null, { status: 204 }));
	}
}

async function open(search = '') {
	app.page.url = new URL(`http://localhost/instances/inst-a/access${search}`);
	session.permissions = held;
	render(Page);
	await screen.findByRole('heading', { name: 'People with access' });
}

const card = (name: string) => {
	const root = screen
		.getByRole('heading', { name, level: 3 })
		.closest<HTMLElement>('[data-slot="card"]');
	if (!root) throw new Error(`no card for ${name}`);
	return root;
};

/** The Give access form: the card that holds its submit button. */
const form = () => {
	const root = screen
		.getByRole('button', { name: 'Give access' })
		.closest<HTMLElement>('[data-slot="card"]');
	if (!root) throw new Error('no give access form');
	return root;
};

const extra = (root: HTMLElement, name: RegExp) => within(root).getByRole('checkbox', { name });

async function options(trigger: HTMLElement): Promise<string[]> {
	await fireEvent.pointerDown(trigger, { button: 0, pointerType: 'mouse' });
	return (await screen.findAllByRole('option')).map((option) => text(option).trim());
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	serve();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('unsaved edits while another person changes', () => {
	it('keeps an unsaved edit for one person when another is saved', async () => {
		await open();
		await click(extra(card('alice'), /manage mods/i));
		await click(extra(card('bob'), /restore backups/i));

		await click(within(card('bob')).getByRole('button', { name: /save access/i }));
		await vi.waitFor(() => expect(daemon.requests('PUT', `${path}/u-b`)).toHaveLength(1));
		await vi.waitFor(() => expect(within(card('bob')).queryByText('Unsaved changes')).toBeNull());

		expect(daemon.requests('PUT', `${path}/u-a`)).toHaveLength(0);
		expect(extra(card('alice'), /manage mods/i)).toHaveProperty('checked', true);
		expect(within(card('alice')).getByText('Unsaved changes')).toBeTruthy();
		expect(daemon.requests('GET', path)).toHaveLength(1);
	});

	it('keeps an unsaved edit for one person when another is revoked', async () => {
		await open();
		await click(extra(card('alice'), /manage mods/i));

		await click(within(card('bob')).getByRole('button', { name: /revoke access/i }));
		await vi.waitFor(() => expect(screen.queryByRole('heading', { name: 'bob' })).toBeNull());

		expect(daemon.requests('DELETE', `${path}/u-b`)).toHaveLength(1);
		expect(extra(card('alice'), /manage mods/i)).toHaveProperty('checked', true);
		expect(within(card('alice')).getByText('Unsaved changes')).toBeTruthy();
		expect(await options(form().querySelector<HTMLElement>('#new-user')!)).toContain('bob');
	});

	it('keeps an unsaved edit for one person when access is given to another', async () => {
		await open();
		await click(extra(card('alice'), /manage mods/i));

		await click(within(form()).getByRole('button', { name: 'Give access' }));
		await screen.findByRole('heading', { name: 'carol', level: 3 });

		const [created] = daemon.requests('PUT', `${path}/u-c`);
		expect(created.headers['If-None-Match']).toBe('*');
		expect(created.body).toEqual({ role: 'viewer', perms: [] });
		expect(extra(card('alice'), /manage mods/i)).toHaveProperty('checked', true);
		expect(within(card('alice')).getByText('Unsaved changes')).toBeTruthy();
		expect(text(form().querySelector('#new-user'))).toContain('dave');
	});

	it('discards every edit from the conflict banner', async () => {
		daemon.on('PUT', `${path}/u-b`, () =>
			Response.json(
				{ error: { code: 'stale_write', message: 'Stale.', request_id: 'req-test' } },
				{ status: 412 }
			)
		);
		daemon.on('GET', `${path}/u-b`, () => Response.json(grant('u-b', 'viewer', [], '"v3"')));
		await open();
		await click(extra(card('alice'), /manage mods/i));
		await click(extra(card('bob'), /restore backups/i));
		await click(within(card('bob')).getByRole('button', { name: /save access/i }));

		await click(await screen.findByRole('button', { name: 'Discard all access edits' }));
		await screen.findByRole('heading', { name: 'alice', level: 3 });

		expect(extra(card('alice'), /manage mods/i)).toHaveProperty('checked', false);
		expect(screen.queryByText('Unsaved changes')).toBeNull();
	});

	it('overwrites a stale grant with the current etag and leaves other edits alone', async () => {
		daemon.on('PUT', `${path}/u-b`, (request) =>
			request.headers['If-Match'] === '"v3"'
				? Response.json(grant('u-b', 'operator', ['backups.restore'], '"v4"'))
				: Response.json(
						{ error: { code: 'stale_write', message: 'Stale.', request_id: 'req-test' } },
						{ status: 412 }
					)
		);
		daemon.on('GET', `${path}/u-b`, () =>
			Response.json(grant('u-b', 'viewer', [], '"v3"'), { headers: { ETag: '"v3"' } })
		);
		await open();
		await click(extra(card('alice'), /manage mods/i));
		await click(extra(card('bob'), /restore backups/i));
		await click(within(card('bob')).getByRole('button', { name: /save access/i }));

		await click(await screen.findByRole('button', { name: 'Overwrite saved access' }));
		await vi.waitFor(() => expect(screen.queryByText(/Someone else changed/)).toBeNull());

		expect(daemon.requests('PUT', `${path}/u-b`).map((sent) => sent.headers['If-Match'])).toEqual([
			'"v1"',
			'"v3"'
		]);
		expect(within(card('bob')).queryByText('Unsaved changes')).toBeNull();
		expect(extra(card('alice'), /manage mods/i)).toHaveProperty('checked', true);
	});
});

describe('administrators', () => {
	it('lists them apart from the grants, read-only, and out of the picker', async () => {
		await open();

		const section = screen.getByRole('heading', { name: 'Administrators' }).closest('section')!;
		expect(text(section)).toContain('Administrators can use every server without a grant.');
		expect(within(section).getByText('root')).toBeTruthy();
		expect(within(section).queryByRole('button')).toBeNull();
		expect(screen.queryByRole('heading', { name: 'root', level: 3 })).toBeNull();
		expect(await options(form().querySelector<HTMLElement>('#new-user')!)).toEqual([
			'carol',
			'dave'
		]);
	});

	it('leaves a disabled administrator out of the list', async () => {
		serve([user('u-old', 'olga', { role: 'admin', disabled: true })]);
		await open();

		const section = screen.getByRole('heading', { name: 'Administrators' }).closest('section')!;
		expect(within(section).queryByText('olga')).toBeNull();
	});
});

describe('readable access', () => {
	it('names the base access and describes what it includes', async () => {
		await open();

		const bob = card('bob');
		expect(text(within(bob).getByLabelText('Base access')).trim()).toBe('Operator');
		expect(text(bob)).toContain(
			'Can view the server, console, stats, backups, mods, and configuration; ' +
				'start, stop, and restart the server; create and download backups; ' +
				'manage players; and send console commands.'
		);
		expect(text(card('alice'))).toContain(
			'Can view the server, console, stats, backups, mods, and configuration.'
		);
	});

	it('shows extra capabilities by label with the risk beneath', async () => {
		await open();

		const box = extra(card('alice'), /manage mods/i);
		const label = box.closest('label')!;
		expect(text(label)).toContain('Manage mods');
		expect(text(label)).toContain(
			'Can install, update, and extract arbitrary third-party archives.'
		);
		expect(text(label)).not.toContain('mods.manage');
		expect(label.getAttribute('title')).toBeNull();
	});

	it('updates the effective access with the draft and flags unsaved changes', async () => {
		await open();
		const alice = card('alice');
		expect(within(alice).queryByText('Unsaved changes')).toBeNull();

		await click(extra(alice, /manage mods/i));
		expect(text(alice)).toContain(
			'Effective access: Can view the server, console, stats, backups, mods, and configuration; and manage mods.'
		);
		expect(within(alice).getByText('Unsaved changes')).toBeTruthy();

		await click(extra(alice, /manage mods/i));
		expect(within(alice).queryByText('Unsaved changes')).toBeNull();
	});

	it('describes the access being given as it is chosen', async () => {
		await open();
		const give = form();

		expect(text(give)).toContain(
			'Effective access: Can view the server, console, stats, backups, mods, and configuration.'
		);
		await click(extra(give, /restore backups/i));
		expect(text(give)).toContain('restore backups.');
	});
});

describe('the Give access form', () => {
	it('selects a person whose grant was just revoked when nobody else could be chosen', async () => {
		daemon.on('GET', '/users', () =>
			Response.json({
				items: [user('u-root', 'root', { role: 'admin' }), user('u-a', 'alice'), user('u-b', 'bob')]
			})
		);
		await open();
		expect(screen.queryByRole('button', { name: 'Give access' })).toBeNull();

		await click(within(card('bob')).getByRole('button', { name: /revoke access/i }));
		await vi.waitFor(() =>
			expect(screen.queryByRole('heading', { name: 'bob', level: 3 })).toBeNull()
		);

		expect(text(form().querySelector('#new-user')).trim()).toBe('bob');
		expect(screen.getByRole('button', { name: 'Give access' })).toHaveProperty('disabled', false);
	});
});

describe('the ?user= link', () => {
	const selected = () => text(form().querySelector('#new-user')).trim();

	it('preselects that person in the Give access form', async () => {
		await open('?user=u-d');
		expect(selected()).toBe('dave');
	});

	it.each([
		['an administrator', 'u-root'],
		['someone who already has a grant', 'u-a'],
		['nobody', 'u-missing']
	])('ignores %s', async (_name, id) => {
		await open(`?user=${id}`);
		expect(selected()).toBe('carol');
	});
});

describe('the invite link', () => {
	const name = 'Invite someone new to this server';

	it('opens the invites page with this server chosen for someone who manages invites', async () => {
		await open();
		session.permissions = permissions('inst-a', [], [actions.grantsManage, actions.invitesManage]);

		const link = await screen.findByRole('link', { name });
		expect(link.getAttribute('href')).toBe('/admin/invites?instance=inst-a');
	});

	it('is absent for someone who cannot manage invites', async () => {
		await open();
		expect(screen.queryByRole('link', { name })).toBeNull();
	});
});
