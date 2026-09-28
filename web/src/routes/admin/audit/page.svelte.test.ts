import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { page } from '$app/state';
import type { AuditEntry, AuditFilters, AuditPage } from '$lib/api/admin';
import { instanceList } from '$lib/state/instances.svelte';
import { FakeDaemon, envelope, instance, type Sent } from '$lib/testing/daemon';
import { choose, click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

// The address is a reactive URL and goto rewrites it, so the screen sees its own navigation
// the way SvelteKit would report it.
vi.mock('$app/state', async () => {
	const { SvelteURL } = await import('svelte/reactivity');
	return { page: { url: new SvelteURL('http://localhost/admin/audit') } };
});
vi.mock('$app/navigation', async () => {
	const { page } = await import('$app/state');
	return {
		goto: vi.fn(async (to: URL | string) => {
			page.url.href = new URL(String(to), page.url).href;
		})
	};
});
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
	return {
		id: 'a-1',
		user_id: 'u-alex',
		actor: 'Alex',
		instance_id: 'inst-a',
		instance: 'Viking World',
		action: 'instances.start',
		detail: '{}',
		ip: '203.0.113.7',
		outcome: 'succeeded',
		job_id: null,
		job_error: null,
		created_at: '2026-09-20T12:00:00Z',
		...overrides
	};
}

const facets: AuditFilters = {
	actions: ['instances.start', 'instances.mods.install', 'users.password.reset'],
	actors: [
		{ id: 'u-alex', name: 'Alex' },
		{ id: 'u-gone', name: 'Bob' }
	],
	instances: [
		{ id: 'inst-gone', name: 'Old World' },
		{ id: 'inst-a', name: 'Viking World' }
	]
};

const rows = (items: AuditEntry[], next_cursor: string | null = null): Response =>
	Response.json({ items, next_cursor } satisfies AuditPage);

function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((done) => (resolve = done));
	return { promise, resolve };
}

/** Records the signal of every request that leaves, so a test can see which were aborted. */
function signals(): AbortSignal[] {
	const seen: AbortSignal[] = [];
	const inner = globalThis.fetch;
	vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
		if (init?.signal) seen.push(init.signal);
		return inner(input, init);
	});
	return seen;
}

const auditRequests = () => daemon.requests('GET', '/audit');

async function open(
	search = '',
	audit: (request: Sent) => Response | Promise<Response> = () => rows([entry()])
) {
	page.url.href = `http://localhost/admin/audit${search}`;
	daemon.on('GET', '/audit', audit);
	daemon.on('GET', '/audit/filters', () => Response.json(facets));
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: [instance({ id: 'inst-a', name: 'Viking World' })],
			next_cursor: null
		})
	);
	render(Page);
	await vi.waitFor(() => expect(auditRequests()).toHaveLength(1));
}

async function optionsOf(label: string): Promise<string[]> {
	await fireEvent.pointerDown(screen.getByLabelText(label), { button: 0, pointerType: 'mouse' });
	const options = await screen.findAllByRole('option');
	const names = options.map((o) => o.textContent?.trim() ?? '');
	await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
	return names;
}

const exportHref = () => screen.getByRole('link', { name: 'Export CSV' }).getAttribute('href');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	page.url.href = 'http://localhost/admin/audit';
	vi.mocked(goto).mockClear();
});

afterEach(() => {
	socket.reset();
	instanceList.release();
	vi.unstubAllGlobals();
});

describe('the audit screen filters', () => {
	it('offer every action, actor and server the trail holds, including ones that are gone', async () => {
		await open();

		expect(await optionsOf('Actor')).toEqual(['Anyone', 'Alex', 'Bob']);
		expect(await optionsOf('Server')).toEqual(['Any server', 'Old World', 'Viking World']);
		expect(await optionsOf('Action')).toEqual([
			'Any action',
			'Mod installed',
			'Server started',
			'Users password reset'
		]);
		expect(daemon.requests('GET', '/audit/filters')).toHaveLength(1);
	});

	it('do not depend on the rows loaded', async () => {
		await open('', () => rows([]));

		expect(await optionsOf('Actor')).toEqual(['Anyone', 'Alex', 'Bob']);
	});

	it('refetch with the chosen filter and keep it in the address', async () => {
		await open();

		await choose(screen.getByLabelText('Action'), 'Mod installed');

		await vi.waitFor(() => expect(auditRequests()).toHaveLength(2));
		expect(auditRequests()[1].query.get('action')).toBe('instances.mods.install');
		const [url, how] = vi.mocked(goto).mock.calls[0];
		expect(new URL(String(url)).searchParams.get('action')).toBe('instances.mods.install');
		expect(how).toEqual({ replaceState: true, keepFocus: true, noScroll: true });
		expect(page.url.search).toBe('?action=instances.mods.install');
	});

	it('send the actor and the server by id', async () => {
		await open();

		await choose(screen.getByLabelText('Actor'), 'Bob');
		await choose(screen.getByLabelText('Server'), 'Old World');

		await vi.waitFor(() => expect(auditRequests()).toHaveLength(3));
		const last = auditRequests()[2].query;
		expect(last.get('user_id')).toBe('u-gone');
		expect(last.get('instance_id')).toBe('inst-gone');
	});

	it('drop a filter when it is set back to any', async () => {
		await open('?action=instances.start');

		await choose(screen.getByLabelText('Action'), 'Any action');

		await vi.waitFor(() => expect(auditRequests()).toHaveLength(2));
		expect(auditRequests()[1].query.has('action')).toBe(false);
		expect(page.url.search).toBe('');
	});

	it('read their starting values from the address', async () => {
		await open('?action=instances.mods.install&user_id=u-gone&from=2026-09-01&to=2026-09-03');

		const query = auditRequests()[0].query;
		expect(query.get('action')).toBe('instances.mods.install');
		expect(query.get('user_id')).toBe('u-gone');
		expect(query.get('since')).toBe('2026-09-01T00:00:00Z');
		expect(query.get('until')).toBe('2026-09-04T00:00:00Z');
		expect(screen.getByLabelText('Action').textContent?.trim()).toBe('Mod installed');
		expect((screen.getByLabelText('From (UTC date)') as HTMLInputElement).value).toBe('2026-09-01');
		expect((screen.getByLabelText('To (UTC date)') as HTMLInputElement).value).toBe('2026-09-03');
		await vi.waitFor(() => expect(screen.getByLabelText('Actor').textContent?.trim()).toBe('Bob'));
	});

	it('ignore a date in the address that is not one', async () => {
		await open('?from=soon&to=2026-13-45');

		const query = auditRequests()[0].query;
		expect(query.has('since')).toBe(false);
		expect(query.has('until')).toBe(false);
	});

	it('turn the dates into a UTC window and write them to the address', async () => {
		await open();

		await fireEvent.change(screen.getByLabelText('From (UTC date)'), {
			target: { value: '2026-09-01' }
		});
		await vi.waitFor(() => expect(auditRequests()).toHaveLength(2));
		expect(auditRequests()[1].query.get('since')).toBe('2026-09-01T00:00:00Z');
		expect(auditRequests()[1].query.has('until')).toBe(false);

		await fireEvent.change(screen.getByLabelText('To (UTC date)'), {
			target: { value: '2026-09-30' }
		});
		await vi.waitFor(() => expect(auditRequests()).toHaveLength(3));
		expect(auditRequests()[2].query.get('until')).toBe('2026-10-01T00:00:00Z');
		expect(page.url.search).toBe('?from=2026-09-01&to=2026-09-30');

		await fireEvent.change(screen.getByLabelText('From (UTC date)'), { target: { value: '' } });
		await vi.waitFor(() => expect(auditRequests()).toHaveLength(4));
		expect(auditRequests()[3].query.has('since')).toBe(false);
		expect(page.url.search).toBe('?to=2026-09-30');
	});

	it('are what the export carries', async () => {
		await open('?action=instances.start&from=2026-09-01&to=2026-09-01');

		const link = screen.getByRole('link', { name: 'Export CSV' });
		expect(link.hasAttribute('download')).toBe(true);
		const href = new URL(exportHref() ?? '', 'http://localhost');
		expect(href.pathname).toBe('/api/v1/audit/export');
		expect(href.searchParams.get('action')).toBe('instances.start');
		expect(href.searchParams.get('since')).toBe('2026-09-01T00:00:00Z');
		expect(href.searchParams.get('until')).toBe('2026-09-02T00:00:00Z');

		await choose(screen.getByLabelText('Action'), 'Any action');
		await vi.waitFor(() =>
			expect(new URL(exportHref() ?? '', 'http://localhost').searchParams.has('action')).toBe(false)
		);
	});

	it('leave the export unfiltered when none is set', async () => {
		await open();

		expect(exportHref()).toBe('/api/v1/audit/export');
	});
});

describe('the audit screen rows', () => {
	it('say what happened in words, with the outcome and a link to a server that still exists', async () => {
		await open('', () =>
			rows([
				entry({
					action: 'instances.mods.install',
					detail: JSON.stringify({ full_name: 'ExampleMod', from: '2.4.0', to: '2.3.1' })
				})
			])
		);

		const summary = await screen.findByText(
			'Alex changed ExampleMod from 2.4.0 to 2.3.1 on Viking World'
		);
		const row = summary.closest('tr') as HTMLElement;
		expect(within(row).getByText('Completed')).toBeTruthy();
		expect(within(row).getByText('2026-09-20 12:00:00 UTC')).toBeTruthy();
		await vi.waitFor(() =>
			expect(within(row).getByRole('link', { name: 'Viking World' }).getAttribute('href')).toBe(
				'/instances/inst-a'
			)
		);
	});

	it('label a deleted user and server and keep their ids and details in the expansion', async () => {
		await open('', () =>
			rows([
				entry({
					id: 'a-gone',
					user_id: 'u-gone',
					actor: null,
					instance_id: 'inst-gone',
					instance: null,
					detail: '{"note":"kept"}',
					ip: null
				})
			])
		);

		const summary = await screen.findByText('Deleted user started a deleted server');
		const row = summary.closest('tr') as HTMLElement;
		expect(within(row).getAllByText('deleted user')).toHaveLength(1);
		expect(within(row).getByText('deleted server')).toBeTruthy();
		expect(within(row).queryByRole('link')).toBeNull();

		const expanded = text(summary.closest('details'));
		expect(expanded).toContain('Actor ID u-gone');
		expect(expanded).toContain('Server ID inst-gone');
		expect(expanded).toContain('Entry ID a-gone');
		expect(expanded).toContain('IP —');
		expect(expanded).toContain('{"note":"kept"}');
	});

	it('keep the name a server had, unlinked and marked, once it is gone', async () => {
		await open('', () => rows([entry({ instance_id: 'inst-gone', instance: 'Old World' })]));

		const summary = await screen.findByText('Alex started Old World');
		const row = summary.closest('tr') as HTMLElement;
		await vi.waitFor(() => expect(text(row)).toContain('(deleted)'));
		expect(within(row).queryByRole('link')).toBeNull();
	});

	it('show no server and no outcome when an entry has neither', async () => {
		await open('', () => rows([entry({ instance_id: null, instance: null, outcome: null })]));

		const row = (await screen.findByText('Alex started a server')).closest('tr') as HTMLElement;
		expect(within(row).queryByText('Completed')).toBeNull();
		expect(text(row)).not.toContain('deleted');
	});

	it('expand to the raw action, the changes and the job that ran', async () => {
		await open('', () =>
			rows([
				entry({
					action: 'instances.settings.update',
					detail: JSON.stringify({
						changes: [
							{ field: 'mem_limit_mb', from: 4096, to: 8192 },
							{ field: 'password', secret: true }
						]
					}),
					outcome: 'failed',
					job_id: 'j-9',
					job_error: 'The disk is full.'
				})
			])
		);

		const summary = await screen.findByText(
			'Alex changed mem_limit_mb from 4096 to 8192 and the password on Viking World'
		);
		const row = summary.closest('tr') as HTMLElement;
		expect(row.querySelector('[data-slot="badge"]')?.textContent?.trim()).toBe('Failed');
		const expanded = text(summary.closest('details'));
		expect(expanded).toContain('Action instances.settings.update');
		expect(expanded).toContain('mem_limit_mb: 4096 → 8192');
		expect(expanded).toContain('password changed (value not recorded)');
		expect(expanded).toContain('Job j-9');
		expect(expanded).toContain('Job status Failed');
		expect(expanded).toContain('Job error The disk is full.');
	});

	it('show an entry it cannot read as its action name and the detail as stored', async () => {
		await open('', () =>
			rows([entry({ action: 'instances.configs.write', detail: 'example.cfg, 12 bytes' })])
		);

		expect(await screen.findByText('Alex: Config file edited: example.cfg, 12 bytes')).toBeTruthy();
	});

	it('say so when nothing matches, or nothing was ever recorded', async () => {
		await open('', () => rows([]));
		expect(await screen.findByText('Nothing has been recorded yet.')).toBeTruthy();
	});

	it('say so when the filters match nothing', async () => {
		await open('?action=instances.start', () => rows([]));
		expect(await screen.findByText('No entries match these filters.')).toBeTruthy();
	});
});

describe('the audit screen loads', () => {
	it('never lets an older answer overwrite a newer filter', async () => {
		const seen = signals();
		const slow = deferred<Response>();
		await open('', (request) =>
			request.query.has('action')
				? rows([entry({ id: 'new', instance: 'Viking World' })])
				: slow.promise
		);

		await choose(screen.getByLabelText('Action'), 'Server started');
		expect(await screen.findByText('Alex started Viking World')).toBeTruthy();

		slow.resolve(rows([entry({ id: 'old', actor: 'Stale', action: 'instances.stop' })]));
		await new Promise((done) => setTimeout(done, 20));

		expect(screen.queryByText('Stale stopped Viking World')).toBeNull();
		expect(screen.getByText('Alex started Viking World')).toBeTruthy();
		expect(seen[0].aborted).toBe(true);
		expect(screen.queryByText('Loading…')).toBeNull();
		expect(screen.queryByRole('alert')).toBeNull();
	});

	it('aborts a load older when the filter changes and appends nothing from it', async () => {
		const seen = signals();
		const older = deferred<Response>();
		await open('', (request) => {
			if (request.query.get('cursor')) return older.promise;
			if (request.query.has('action')) return rows([entry({ id: 'fresh', actor: 'Fresh' })], 'c-2');
			return rows([entry({ id: 'first', actor: 'First' })], 'c-1');
		});
		await screen.findByText('First started Viking World');

		await click(screen.getByRole('button', { name: 'Load older' }));
		await vi.waitFor(() => expect(auditRequests()).toHaveLength(2));
		expect(auditRequests()[1].query.get('cursor')).toBe('c-1');
		expect(text(document.body)).toContain('Loading…');

		await choose(screen.getByLabelText('Action'), 'Server started');
		expect(await screen.findByText('Fresh started Viking World')).toBeTruthy();
		expect(seen[1].aborted).toBe(true);

		older.resolve(rows([entry({ id: 'older', actor: 'Older' })]));
		await new Promise((done) => setTimeout(done, 20));

		expect(screen.queryByText('Older started Viking World')).toBeNull();
		expect(screen.queryByText('First started Viking World')).toBeNull();
		expect(screen.queryByRole('alert')).toBeNull();
		// The old page's cursor is gone: the new page's own is offered, and the screen is idle.
		expect(screen.queryByText('Loading…')).toBeNull();
		await click(screen.getByRole('button', { name: 'Load older' }));
		await vi.waitFor(() => expect(auditRequests()).toHaveLength(4));
		expect(auditRequests()[3].query.get('cursor')).toBe('c-2');
		expect(auditRequests()[3].query.get('action')).toBe('instances.start');
	});

	it('appends the older page to the current rows', async () => {
		await open('', (request) =>
			request.query.get('cursor')
				? rows([entry({ id: 'older', actor: 'Older' })])
				: rows([entry({ id: 'first', actor: 'First' })], 'c-1')
		);
		await screen.findByText('First started Viking World');

		await click(screen.getByRole('button', { name: 'Load older' }));

		expect(await screen.findByText('Older started Viking World')).toBeTruthy();
		expect(screen.getByText('First started Viking World')).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Load older' })).toBeNull();
	});

	it('refreshes the rows and the filters, and aborts a load older in flight', async () => {
		const seen = signals();
		const older = deferred<Response>();
		let calls = 0;
		await open('', (request) => {
			if (request.query.get('cursor')) return older.promise;
			calls++;
			return calls === 1
				? rows([entry({ id: 'first', actor: 'First' })], 'c-1')
				: rows([entry({ id: 'second', actor: 'Second' })]);
		});
		await screen.findByText('First started Viking World');
		await click(screen.getByRole('button', { name: 'Load older' }));
		await vi.waitFor(() => expect(auditRequests()).toHaveLength(2));

		await click(screen.getByRole('button', { name: 'Refresh' }));

		expect(await screen.findByText('Second started Viking World')).toBeTruthy();
		expect(seen[1].aborted).toBe(true);
		expect(daemon.requests('GET', '/audit/filters')).toHaveLength(2);

		older.resolve(rows([entry({ id: 'older', actor: 'Older' })]));
		await new Promise((done) => setTimeout(done, 20));
		expect(screen.queryByText('Older started Viking World')).toBeNull();
		expect(screen.queryByText('Loading…')).toBeNull();
	});

	it('shows a failed load, and clears it when the next load succeeds', async () => {
		let fail = true;
		await open('', () =>
			fail ? envelope(500, 'internal', 'The audit log could not be read.') : rows([entry()])
		);
		expect(await screen.findByText('The audit log could not be read.')).toBeTruthy();
		expect(screen.queryByText('Loading…')).toBeNull();

		fail = false;
		await click(screen.getByRole('button', { name: 'Refresh' }));

		expect(await screen.findByText('Alex started Viking World')).toBeTruthy();
		expect(screen.queryByText('The audit log could not be read.')).toBeNull();
	});

	it('still loads the trail when the filter vocabulary cannot be read', async () => {
		await open();
		daemon.on('GET', '/audit/filters', () => envelope(500, 'internal', 'Filters are unavailable.'));

		await click(screen.getByRole('button', { name: 'Refresh' }));

		expect(await screen.findByText('Filters are unavailable.')).toBeTruthy();
		expect(screen.getByText('Alex started Viking World')).toBeTruthy();
	});
});
