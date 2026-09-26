import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance, type Orphan } from '$lib/api/instances';
import type { InboxItem } from '$lib/api/inbox';
import type { MyPermissions } from '$lib/api/types';
import { instanceList } from '$lib/state/instances.svelte';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance, job } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function grants(global: string[], onA: string[] = [actions.view]): MyPermissions {
	return {
		user_id: 'u-test',
		role: 'member',
		allowed_actions: global,
		instances: [{ instance_id: 'inst-a', allowed_actions: onA }]
	};
}

function condition(overrides: Partial<InboxItem> = {}): InboxItem {
	return {
		kind: 'low_disk',
		severity: 'warning',
		instance_id: 'inst-a',
		since_at: '2026-09-24T10:00:00Z',
		...overrides
	};
}

async function open(
	permissions: MyPermissions,
	{
		servers = [instance()],
		conditions = [] as InboxItem[],
		orphaned = [] as Orphan[]
	}: { servers?: Instance[]; conditions?: InboxItem[]; orphaned?: Orphan[] } = {}
) {
	daemon.on('GET', '/instances', () => Response.json({ items: servers, next_cursor: null }));
	daemon.on('GET', '/instances/inbox', () =>
		Response.json({ items: conditions, next_cursor: null })
	);
	daemon.on('GET', '/instances/orphans', () =>
		Response.json({ items: orphaned, next_cursor: null })
	);
	session.permissions = permissions;
	render(Page);
	await screen.findByRole('heading', { name: 'Servers' });
	await vi.waitFor(() => expect(daemon.requests('GET', '/instances/inbox')).toHaveLength(1));
}

const card = (name: string) => {
	const root = screen.getByRole('link', { name }).closest<HTMLElement>('[data-slot="card"]');
	if (!root) throw new Error(`no card for ${name}`);
	return root;
};

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	// The list is a module singleton that remembers its subscriptions; release it the way a
	// sign-out does, so the next test subscribes afresh against a fresh socket.
	instanceList.release();
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the server list', () => {
	// F3: what a member may do comes from the actions the daemon sent.
	it('offers a new server only to a holder of instance.create', async () => {
		await open(grants([]));
		await screen.findByRole('link', { name: 'inst-a' });
		expect(screen.queryByRole('link', { name: /New server/ })).toBeNull();
	});

	it('links a holder of instance.create to the wizard', async () => {
		await open(grants([actions.create]));
		expect((await screen.findByRole('link', { name: /New server/ })).getAttribute('href')).toBe(
			'/instances/new'
		);
	});

	// F5: deleting a server names it, and it is typed back before anything is sent. The worlds
	// are kept either way, and the request says so.
	it('deletes a server only after its name is typed back, keeping its worlds', async () => {
		await open(grants([], [actions.view, actions.remove]));
		daemon.on('DELETE', '/instances/inst-a', () => Response.json(job(), { status: 202 }));

		await click(within(card('inst-a')).getByRole('button', { name: 'Delete' }));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('Delete inst-a?');
		expect(text(dialog)).toContain('nothing here deletes a world');
		const confirm = within(dialog).getByRole('button', { name: 'Delete server' });
		await click(confirm);
		expect(daemon.requests('DELETE', '/instances/inst-a')).toHaveLength(0);

		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'inst-a' }
		});
		await click(confirm);
		await vi.waitFor(() => expect(daemon.requests('DELETE', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('DELETE', '/instances/inst-a')[0].query.get('keep_worlds')).toBe('true');
	});

	// Q25: the join code is on the card only while the daemon has one, and follows it live.
	it('shows a crossplay join code on the card, and follows it as it changes', async () => {
		await open(grants([]), {
			servers: [instance({ state: 'running', crossplay: true, crossplay_join_code: '111222' })]
		});

		await screen.findByRole('link', { name: 'inst-a' });
		expect(within(card('inst-a')).getByText('111222')).toBeTruthy();
		socket.push('instance.inst-a.state', { type: 'join_code', instance: 'inst-a', code: '333444' });
		await vi.waitFor(() => expect(within(card('inst-a')).getByText('333444')).toBeTruthy());
		socket.push('instance.inst-a.state', { type: 'join_code', instance: 'inst-a', code: null });
		await vi.waitFor(() => expect(within(card('inst-a')).queryByText('333444')).toBeNull());
	});
});

// ADR-195: a server's conditions sit on its card, and what has no card sits in one band.
describe('the server list conditions', () => {
	it('puts a server’s conditions on its card and host conditions in the band', async () => {
		await open(grants([]), {
			conditions: [
				condition({ kind: 'low_disk', instance_id: 'inst-a' }),
				condition({ kind: 'stale_backup', instance_id: null, severity: 'critical' })
			]
		});

		await screen.findByRole('link', { name: 'inst-a' });
		const onCard = text(card('inst-a'));
		const outside = text(document.body).replace(onCard, '');
		expect(onCard).toMatch(/disk/i);
		expect(outside).toMatch(/backup/i);
		expect(onCard).not.toMatch(/backup/i);
	});

	// An attention summary that could not load must not read as a clear one.
	it('says the conditions could not be read, and offers a retry', async () => {
		daemon.on('GET', '/instances/inbox', () => envelope(500, 'internal', 'Broken.'));
		daemon.on('GET', '/instances', () => Response.json({ items: [instance()], next_cursor: null }));
		session.permissions = grants([]);
		render(Page);

		expect(
			await screen.findByText(
				'Conditions could not be read, so this list does not say what needs attention.'
			)
		).toBeTruthy();
		expect(screen.queryByText(/Conditions checked/)).toBeNull();
		daemon.on('GET', '/instances/inbox', () => Response.json({ items: [], next_cursor: null }));
		await click(screen.getByRole('button', { name: 'Retry' }));
		expect(await screen.findByText(/Conditions checked/)).toBeTruthy();
	});
});

// 08 §6.1: an orphan has no instance row, so the list is the only place it can appear — and
// only for a holder of instance.adopt, who is the only one who can act on it.
describe('the unclaimed containers', () => {
	const orphan: Orphan = {
		container_id: 'c0ffee',
		name: 'valmin-old-server',
		instance_id: 'old-server',
		base_port: 2470,
		running: false
	};

	it('are not looked for without instance.adopt', async () => {
		await open(grants([actions.create]), { orphaned: [orphan] });
		expect(daemon.requests('GET', '/instances/orphans')).toHaveLength(0);
		expect(screen.queryByText(/not claimed by any server/)).toBeNull();
	});

	it('are listed with a way to recover each one', async () => {
		await open(grants([actions.adopt]), { orphaned: [orphan] });

		expect(await screen.findByText(/1 container is not claimed by any server/)).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Review and recover' }).getAttribute('href')).toBe(
			'/instances/adopt/c0ffee'
		);
	});

	// No orphans and no answer are different facts.
	it('say the scan failed rather than that there are none', async () => {
		daemon.on('GET', '/instances/orphans', () => envelope(500, 'internal', 'Broken.'));
		daemon.on('GET', '/instances', () => Response.json({ items: [], next_cursor: null }));
		daemon.on('GET', '/instances/inbox', () => Response.json({ items: [], next_cursor: null }));
		session.permissions = grants([actions.adopt]);
		render(Page);

		expect(
			await screen.findByText(
				'Unclaimed containers could not be scanned, so this list does not say whether any exist.'
			)
		).toBeTruthy();
	});
});
