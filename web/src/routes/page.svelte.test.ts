import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance, type Orphan } from '$lib/api/instances';
import type { InboxItem } from '$lib/api/inbox';
import type { MyPermissions } from '$lib/api/types';
import { instanceList } from '$lib/state/instances.svelte';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance } from '$lib/testing/daemon';
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

	it('says there are no servers yet, and invites a holder of instance.create to make one', async () => {
		await open(grants([actions.create]), { servers: [] });
		expect(await screen.findByText('No servers yet. Create one to get started.')).toBeTruthy();
	});

	it('links a holder of instance.create to the wizard', async () => {
		await open(grants([actions.create]));
		expect((await screen.findByRole('link', { name: /New server/ })).getAttribute('href')).toBe(
			'/instances/new'
		);
	});

	it('offers no delete on a server card, which lives in the server settings', async () => {
		await open(grants([], [actions.view, actions.remove]));
		await screen.findByRole('link', { name: 'inst-a' });
		expect(within(card('inst-a')).queryByRole('button', { name: 'Delete' })).toBeNull();
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

describe('refreshing the server list', () => {
	it('re-reads the list and the conditions, and waits while it does', async () => {
		await open(grants([]));
		await screen.findByText(/Conditions checked/);

		let answer!: () => void;
		const answered = new Promise<void>((resolve) => (answer = resolve));
		daemon.on('GET', '/instances/inbox', async () => {
			await answered;
			return Response.json({ items: [condition()], next_cursor: null });
		});
		const refresh = screen.getByRole('button', { name: 'Refresh' });
		await click(refresh);

		expect(refresh.hasAttribute('disabled')).toBe(true);
		await vi.waitFor(() => expect(daemon.requests('GET', '/instances/inbox')).toHaveLength(2));
		expect(daemon.requests('GET', '/instances')).toHaveLength(2);
		answer();
		await vi.waitFor(() => expect(refresh.hasAttribute('disabled')).toBe(false));
		expect(text(card('inst-a'))).toMatch(/low disk/);
	});
});

describe('the conditions while the page stays open', () => {
	let visibility: DocumentVisibilityState = 'visible';
	const reads = () => daemon.requests('GET', '/instances/inbox').length;
	const show = (state: DocumentVisibilityState) => {
		visibility = state;
		document.dispatchEvent(new Event('visibilitychange'));
	};

	beforeEach(() => {
		visibility = 'visible';
		Object.defineProperty(document, 'visibilityState', {
			configurable: true,
			get: () => visibility
		});
		vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] });
	});

	afterEach(() => {
		vi.useRealTimers();
		Reflect.deleteProperty(document, 'visibilityState');
	});

	it('are re-read every minute while the page is visible', async () => {
		await open(grants([]));
		daemon.on('GET', '/instances/inbox', () =>
			Response.json({ items: [condition()], next_cursor: null })
		);

		await vi.advanceTimersByTimeAsync(60_000);
		await vi.waitFor(() => expect(text(card('inst-a'))).toMatch(/low disk/));
		expect(reads()).toBe(2);
		expect(daemon.requests('GET', '/instances')).toHaveLength(1);
	});

	it('pause while hidden, and are re-read on return once the last check has aged', async () => {
		await open(grants([]));

		show('hidden');
		await vi.advanceTimersByTimeAsync(90_000);
		expect(reads()).toBe(1);

		show('visible');
		await vi.waitFor(() => expect(reads()).toBe(2));
	});

	it('are not re-read on return while the last check is recent', async () => {
		await open(grants([]));

		show('hidden');
		await vi.advanceTimersByTimeAsync(30_000);
		show('visible');
		await vi.advanceTimersByTimeAsync(0);
		expect(reads()).toBe(1);

		await vi.advanceTimersByTimeAsync(30_000);
		await vi.waitFor(() => expect(reads()).toBe(2));
	});

	// A failed read must not leave the page reading as all-clear.
	it('report a failed periodic read rather than a clear list', async () => {
		await open(grants([]));
		await screen.findByText(/Conditions checked/);
		daemon.on('GET', '/instances/inbox', () => envelope(500, 'internal', 'Broken.'));

		await vi.advanceTimersByTimeAsync(60_000);
		expect(
			await screen.findByText(
				'Conditions could not be read, so this list does not say what needs attention.'
			)
		).toBeTruthy();
		expect(screen.queryByText(/Conditions checked/)).toBeNull();
	});
});

describe('searching the server list', () => {
	const numbered = (count: number) =>
		Array.from({ length: count }, (_, i) => instance({ id: `inst-${i}`, name: `Server ${i}` }));

	it('offers no search or state filter at six servers', async () => {
		await open(grants([]), { servers: numbered(6) });
		await screen.findByRole('link', { name: 'Server 0' });
		expect(screen.queryByLabelText('Search by name')).toBeNull();
		expect(screen.queryByLabelText('State')).toBeNull();
		expect(screen.queryByText(/of 6 servers/)).toBeNull();
	});

	it('searches by name and filters by state above six servers', async () => {
		await open(grants([]), {
			servers: [
				...numbered(5),
				instance({ id: 'inst-keep', name: 'Broken Keep', state: 'error' }),
				instance({ id: 'inst-hall', name: 'Busy Hall', state: 'running' })
			],
			conditions: [condition({ instance_id: 'inst-1' })]
		});
		await screen.findByRole('link', { name: 'Server 0' });
		expect(screen.getByText('7 of 7 servers')).toBeTruthy();

		const search = screen.getByLabelText('Search by name');
		await fireEvent.input(search, { target: { value: 'KEEP' } });
		expect(screen.getByText('1 of 7 servers')).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Broken Keep' })).toBeTruthy();
		expect(screen.queryByRole('link', { name: 'Server 0' })).toBeNull();

		await fireEvent.input(search, { target: { value: '' } });
		const state = screen.getByLabelText('State');
		await fireEvent.change(state, { target: { value: 'attention' } });
		expect(screen.getByText('2 of 7 servers')).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Server 1' })).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Broken Keep' })).toBeTruthy();

		await fireEvent.change(state, { target: { value: 'running' } });
		await fireEvent.input(search, { target: { value: 'server' } });
		expect(screen.getByText('0 of 7 servers')).toBeTruthy();
		expect(screen.getByText('No servers match')).toBeTruthy();

		await click(screen.getByRole('button', { name: 'Clear filters' }));
		expect(screen.getByText('7 of 7 servers')).toBeTruthy();
		expect((search as HTMLInputElement).value).toBe('');
		expect((state as HTMLSelectElement).value).toBe('all');
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
