import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Grant, GrantPage, GrantRole } from '$lib/api/grants';
import type { Role, User } from '$lib/api/types';
import { FakeDaemon, envelope, instance } from '$lib/testing/daemon';
import { choose, click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

let daemon: FakeDaemon;
let users: User[];

function user(id: string, username: string, overrides: Partial<User> = {}): User {
	return {
		id,
		username,
		role: 'member',
		disabled: false,
		owner: false,
		created_at: '2026-09-01T00:00:00Z',
		last_login_at: null,
		...overrides
	};
}

function grant(userId: string, instanceId: string, role: GrantRole): Grant {
	return {
		user_id: userId,
		instance_id: instanceId,
		role,
		perms: [],
		granted_by: 'u-root',
		granted_at: '2026-09-01T00:00:00Z',
		expires_at: null,
		etag: '"v1"'
	};
}

const grantPage = (items: Grant[]): Response =>
	Response.json({
		items,
		next_cursor: null,
		roles: [],
		extra_capabilities: []
	} satisfies GrantPage);

/** Serves two servers, Alpha and Beta, and four people: alice reaches both, bob only Beta,
 * carol neither, and root is the owning administrator. */
function serve() {
	users = [
		user('u-root', 'root', { role: 'admin', owner: true }),
		user('u-a', 'alice'),
		user('u-b', 'bob'),
		user('u-c', 'carol')
	];
	daemon.on('GET', '/users', () => Response.json({ items: users }));
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: [instance({ id: 'inst-a', name: 'Alpha' }), instance({ id: 'inst-b', name: 'Beta' })],
			next_cursor: null
		})
	);
	daemon.on('GET', '/instances/inst-a/grants', () =>
		grantPage([grant('u-a', 'inst-a', 'operator')])
	);
	daemon.on('GET', '/instances/inst-b/grants', () =>
		grantPage([grant('u-a', 'inst-b', 'viewer'), grant('u-b', 'inst-b', 'viewer')])
	);
	daemon.on('POST', '/users', (request) => {
		const body = request.body as { username: string; role: Role };
		const created = user('u-new', body.username, { role: body.role });
		users.push(created);
		return Response.json(created, { status: 201 });
	});
}

async function open() {
	render(Page);
	await screen.findByRole('heading', { name: 'Existing users' });
	await vi.waitFor(() => expect(screen.getAllByText('Server access:').length).toBeGreaterThan(0));
}

/** The card of one existing user, found by the name in its title. */
function card(name: string): HTMLElement {
	const title = screen
		.getAllByRole('heading', { level: 2 })
		.find((heading) => text(heading).trim().startsWith(name));
	const root = title?.closest<HTMLElement>('[data-slot="card"]');
	if (!root) throw new Error(`no card for ${name}`);
	return root;
}

const links = (root: HTMLElement) =>
	within(root)
		.queryAllByRole('link')
		.map((link) => [text(link), link.getAttribute('href')]);

/** Creates a user through the form, as the given role. */
async function create(username: string, role: 'Member' | 'Administrator') {
	await fireEvent.input(screen.getByLabelText('Username'), { target: { value: username } });
	if (role !== 'Member') await choose(document.querySelector<HTMLElement>('#new-role')!, role);
	await click(screen.getByRole('button', { name: 'Create user' }));
	await screen.findByText('Copy this password now');
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	serve();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('server access on each user', () => {
	it('names the servers a member reaches with the base role, each linking to its access page', async () => {
		await open();

		const alice = card('alice');
		expect(text(alice)).toContain('Server access: Alpha (operator), Beta (viewer)');
		expect(links(alice)).toEqual([
			['Alpha', '/instances/inst-a/access?user=u-a'],
			['Beta', '/instances/inst-b/access?user=u-a']
		]);
		expect(text(card('bob'))).toContain('Server access: Beta (viewer)');
	});

	it('says a member has no access yet and links to every server to give it', async () => {
		await open();

		const carol = card('carol');
		expect(text(carol)).toContain('No server access yet. Give server access: Alpha, Beta');
		expect(links(carol)).toEqual([
			['Alpha', '/instances/inst-a/access?user=u-c'],
			['Beta', '/instances/inst-b/access?user=u-c']
		]);
	});

	it('says an administrator reaches every server, with no links', async () => {
		await open();

		const root = card('root');
		expect(text(root)).toContain('Server access: Every server (administrator)');
		expect(links(root)).toEqual([]);
	});

	it('offers no link when no server exists', async () => {
		daemon.on('GET', '/instances', () => Response.json({ items: [], next_cursor: null }));
		await open();

		expect(text(card('carol'))).toContain('No server access yet.');
		expect(text(card('carol'))).not.toContain('Give server access');
		expect(links(card('carol'))).toEqual([]);
	});

	it('shows what loaded and names the server whose grants could not be read', async () => {
		daemon.on('GET', '/instances/inst-b/grants', () => envelope(500, 'internal', 'Failed.'));
		await open();

		expect((await screen.findByRole('status')).textContent).toBe(
			'Access on Beta could not be read, so it may be missing below.'
		);
		expect(text(card('alice'))).toContain('Server access: Alpha (operator)');
		expect(text(card('alice'))).not.toContain('Beta (viewer)');
	});

	it('keeps the user list and says so when the servers cannot be listed', async () => {
		daemon.on('GET', '/instances', () => envelope(500, 'internal', 'Failed.'));
		render(Page);

		expect((await screen.findByRole('status')).textContent).toBe(
			'Server access could not be loaded.'
		);
		expect(text(card('root'))).toContain('Every server (administrator)');
		expect(text(card('alice'))).not.toContain('Server access: No server access');
		expect(links(card('alice'))).toEqual([]);
	});
});

describe('after creating a user', () => {
	it('offers each server as a link that preselects the new member', async () => {
		await open();
		await create('dave', 'Member');

		const panel = screen.getByText('Copy this password now').closest('section')!;
		expect(text(panel)).toContain('Give server access');
		expect(links(panel)).toEqual([
			['Alpha', '/instances/inst-a/access?user=u-new'],
			['Beta', '/instances/inst-b/access?user=u-new']
		]);
		expect(daemon.requests('POST', '/users')[0].body).toMatchObject({
			username: 'dave',
			role: 'member'
		});
	});

	it('says an administrator needs no server access, with no links', async () => {
		await open();
		await create('erin', 'Administrator');

		const panel = screen.getByText('Copy this password now').closest('section')!;
		expect(text(panel)).toContain(
			'Administrators reach every server, so erin needs no server access.'
		);
		expect(text(panel)).not.toContain('Give server access');
		expect(links(panel)).toEqual([]);
	});

	it('says there is nothing to give access to when no server exists', async () => {
		daemon.on('GET', '/instances', () => Response.json({ items: [], next_cursor: null }));
		await open();
		await create('dave', 'Member');

		const panel = screen.getByText('Copy this password now').closest('section')!;
		expect(text(panel)).toContain('No servers exist yet, so there is nothing to give access to.');
		expect(links(panel)).toEqual([]);
	});

	it('leaves the access step out of a password reset', async () => {
		daemon.on('POST', '/users/u-a/password/reset', () => Response.json({ password: 'fresh' }));
		await open();

		await click(within(card('alice')).getByRole('button', { name: 'Reset password' }));
		const panel = (await screen.findByText('Copy this password now')).closest('section')!;

		expect(text(panel)).not.toContain('Give server access');
		expect(text(panel)).not.toContain('needs no server access');
	});
});

describe('role explanations', () => {
	it('describes both roles beside the create form', async () => {
		await open();

		const form = screen.getByRole('button', { name: 'Create user' }).closest('form')!;
		expect(text(form)).toContain(
			'Member: Uses only the servers they are given access to, at the level each grant allows.'
		);
		expect(text(form)).toContain(
			'Administrator: Reaches every server and manages the panel: servers, users, access, settings, schedules, and the audit log.'
		);
	});

	it('explains the role each existing user holds under its select', async () => {
		users.push(user('u-e', 'erin', { role: 'admin' }));
		await open();

		expect(text(card('alice'))).toContain(
			'Uses only the servers they are given access to, at the level each grant allows.'
		);
		expect(text(card('erin'))).toContain('Reaches every server and manages the panel');
		expect(text(card('erin'))).not.toContain('Uses only the servers');
	});
});
