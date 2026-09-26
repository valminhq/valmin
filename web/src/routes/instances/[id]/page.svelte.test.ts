import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type DiskUsage } from '$lib/api/instances';
import type { Job } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a') }
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function disk(overrides: Partial<DiskUsage> = {}): DiskUsage {
	return {
		total_bytes: 3 * 1024 ** 3,
		server_bytes: 2 * 1024 ** 3,
		worlds_bytes: 512 * 1024 ** 2,
		logs_bytes: 0,
		backups_bytes: 512 * 1024 ** 2,
		free_bytes: 40 * 1024 ** 3,
		alarm_bytes: 1024 ** 3,
		low: false,
		server_thresholds: null,
		...overrides
	} as DiskUsage;
}

/** Renders the server page for a member holding `held`, once the server's row has loaded. */
async function open(
	held: string[],
	{
		row = instance(),
		history = [] as Job[],
		channel = 'rcon' as 'rcon' | 'none',
		usage = disk()
	} = {}
) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', '/instances/inst-a/capabilities', () =>
		Response.json({
			command_channel: channel,
			detected: channel !== 'none',
			allowed_commands: [],
			allowed_actions: []
		})
	);
	daemon.on('GET', '/instances/inst-a/jobs', () =>
		Response.json({ items: history, next_cursor: null })
	);
	daemon.on('GET', '/instances/inst-a/operation', () => Response.json(null));
	daemon.on('GET', '/instances/inst-a/disk', () => Response.json(usage));
	daemon.on('GET', '/instances/inst-a/update-status', () =>
		Response.json({
			installed_build_id: '1',
			public_build_id: '1',
			observed_at: null,
			update_available: false
		})
	);
	daemon.on('GET', '/instances/inst-a/stats', () =>
		Response.json({
			available: true,
			ts: '2026-09-24T12:00:00Z',
			cpu_pct: 12,
			mem_bytes: 1024 ** 3,
			mem_limit: 4 * 1024 ** 3,
			mem_pct: 25,
			players: null
		})
	);
	daemon.on('GET', '/instances/inst-a/logs', () => Response.json({ items: [] }));
	session.permissions = permissions('inst-a', held);
	render(Page);
	await screen.findByRole('group', { name: 'Server controls' });
	await vi.waitFor(() =>
		expect(daemon.requests('GET', '/instances/inst-a/operation')).toHaveLength(1)
	);
}

const push = (topic: string, message: Parameters<typeof socket.push>[1]) =>
	socket.push(`instance.inst-a.${topic}`, message);

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the server page', () => {
	// 12 §2.4: `error` is a parking state whose only exit is a human's. Acknowledging re-runs
	// reconciliation, and the daemon decides where the row lands, so the page reads it again.
	it('offers a check on a parked server and re-reads the row the daemon lands on', async () => {
		await open([actions.view, actions.start], {
			row: instance({ state: 'error' }),
			history: [job({ kind: 'start', status: 'failed' })]
		});
		daemon.on('POST', '/instances/inst-a/acknowledge', () =>
			Response.json(instance({ state: 'stopped' }))
		);

		expect(text(screen.getByRole('alert'))).toContain('The last start failed');
		daemon.on('GET', '/instances/inst-a', () => Response.json(instance({ state: 'stopped' })));
		await click(screen.getByRole('button', { name: 'Check this server' }));

		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/acknowledge')).toHaveLength(1)
		);
		await vi.waitFor(() => expect(screen.queryByText('This server needs a check')).toBeNull());
		expect(daemon.requests('GET', '/instances/inst-a')).toHaveLength(2);
	});

	// Q25: the join code is logged seconds after the server is up, so it arrives on the state
	// topic, and a null clears it.
	it('shows the live crossplay join code, and drops it when the daemon clears it', async () => {
		await open([actions.view], { row: instance({ state: 'running', crossplay: true }) });

		push('state', { type: 'join_code', instance: 'inst-a', code: '482913' });
		expect(await screen.findByText('482913')).toBeTruthy();

		push('state', { type: 'join_code', instance: 'inst-a', code: null });
		await vi.waitFor(() => expect(screen.queryByText('482913')).toBeNull());
	});

	// F3, and a clone copies a world at rest: the link is there for a holder of instance.clone
	// and leads anywhere only while the source is stopped.
	it('links to cloning only for a holder of instance.clone, and only while stopped', async () => {
		await open([actions.view, actions.clone]);
		expect(screen.getByText('Clone').closest('a')?.getAttribute('href')).toBe(
			'/instances/inst-a/clone'
		);

		push('state', { type: 'state', instance: 'inst-a', state: 'running', restart_required: false });
		await vi.waitFor(() =>
			expect(screen.getByText('Clone').closest('a')?.hasAttribute('href')).toBe(false)
		);
	});

	it('shows no clone link without instance.clone', async () => {
		await open([actions.view]);
		expect(screen.queryByText('Clone')).toBeNull();
	});

	// E7: the daemon sends null when it cannot tell, and a 0 here would be a number an
	// operator could act on, invented by the panel.
	it('renders an unknown player count as unknown, never as 0', async () => {
		await open([actions.view, actions.statsRead], { row: instance({ state: 'running' }) });

		const players = (await screen.findByText('Players')).nextElementSibling;
		await vi.waitFor(() => expect(text(players).trim()).toBe('unknown'));

		push('stats', {
			type: 'stats',
			instance: 'inst-a',
			ts: '2026-09-24T12:00:02Z',
			cpu_pct: 10,
			mem_bytes: 1024 ** 3,
			mem_limit: 4 * 1024 ** 3,
			mem_pct: 25,
			players: 3
		});
		await vi.waitFor(() => expect(text(players).trim()).toBe('3'));
	});

	// 03 §3.4: below its own floor the server stops saving the world without an error, so free
	// space is on screen, and the daemon's `low` is what raises the alarm.
	it('shows free space, and warns when the daemon calls it low', async () => {
		await open([actions.view, actions.statsRead], {
			usage: disk({ free_bytes: 512 * 1024 ** 2, low: true })
		});

		expect((await screen.findByText('Free')).nextElementSibling?.textContent).toBe('512.0 MiB');
		expect(screen.getByText(/Below 1\.0 GiB free\./)).toBeTruthy();
	});

	it('does not warn when there is room', async () => {
		await open([actions.view, actions.statsRead]);
		await screen.findByText('Free');
		expect(screen.queryByText(/free space here before playing further/)).toBeNull();
	});
});

// E3, 07 §5: commands go through RCON, and the input is live only when RCON is there, the
// member may send, and the server is running — with the reason said when it is not.
describe('the server console', () => {
	const input = () => screen.getByLabelText('Server command') as HTMLInputElement;
	const reason = () => text(document.getElementById('console-input-reason')).trim();
	const console = [actions.view, actions.consoleRead, actions.commandsSend];

	it('takes a command when every prerequisite holds, and sends it', async () => {
		await open(console, { row: instance({ state: 'running' }) });
		daemon.on('POST', '/instances/inst-a/commands', () => Response.json({ output: 'ok' }));

		expect(input().disabled).toBe(false);
		await fireEvent.input(input(), { target: { value: 'save' } });
		await click(screen.getByRole('button', { name: 'Send' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/commands')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/commands')[0].body).toEqual({
			command: 'save'
		});
	});

	it('says RCON is missing when it is', async () => {
		await open(console, { row: instance({ state: 'running' }), channel: 'none' });
		expect(input().disabled).toBe(true);
		expect(reason()).toBe('Install Tristan-ValheimRcon to send commands.');
	});

	it('says the member may not send commands', async () => {
		await open([actions.view, actions.consoleRead], { row: instance({ state: 'running' }) });
		expect(input().disabled).toBe(true);
		expect(reason()).toBe('You do not have permission to send commands.');
	});

	it('says the server has to be running', async () => {
		await open(console);
		expect(input().disabled).toBe(true);
		expect(reason()).toBe('Start the server before sending a command.');
	});

	it('is not shown without console.read', async () => {
		await open([actions.view]);
		expect(screen.queryByLabelText('Server command')).toBeNull();
	});
});
