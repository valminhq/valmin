import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type DiskUsage } from '#lib/api/instances.js';
import type { Operation } from '#lib/api/operations.js';
import type { Job } from '#lib/api/types.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, instance, job, permissions } from '#lib/testing/daemon.js';
import { click, text } from '#lib/testing/interact.js';
import { socket } from '#lib/testing/socket.js';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a') }
}));
vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

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
		next = null as string | null,
		channel = 'rcon' as 'rcon' | 'none',
		usage = disk(),
		operation = null as Operation | null
	} = {}
) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', '/instances/inst-a/capabilities', () =>
		Response.json({
			command_channel: channel,
			detected: channel !== 'none',
			allowed_commands: [],
			allowed_actions: [],
			packages: {
				rcon: 'Tristan-ValheimRcon',
				upgrade_world: 'JereKuusela-Upgrade_World',
				fresh_world: 'sighsorry-FreshWorld'
			}
		})
	);
	daemon.on('GET', '/instances/inst-a/jobs', () =>
		Response.json({ items: history, next_cursor: next })
	);
	daemon.on('GET', '/instances/inst-a/operation', () => Response.json(operation));
	daemon.on('GET', '/instances/inst-a/disk', () => Response.json(usage));
	daemon.on('GET', '/instances/inst-a/update-status', () =>
		Response.json({
			installed_build_id: '1',
			public_build_id: '1',
			branch: 'public',
			branch_build_id: '1',
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
	// `error` is a parking state whose only exit is a human's. Acknowledging re-runs
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

	it('offers only a retry when the install itself failed', async () => {
		await open([actions.view, actions.start, actions.create], {
			row: instance({ state: 'error' }),
			history: [
				job({ kind: 'provision', status: 'failed', error: 'game files are not owned by uid 10000' })
			],
			operation: {
				id: 'op-1',
				kind: 'create',
				state: 'interrupted',
				cursor: 0,
				steps: [{ kind: 'provision' }, { kind: 'start' }],
				created_at: '2026-10-10T08:00:00Z',
				updated_at: '2026-10-10T08:01:00Z'
			}
		});

		expect(await screen.findByRole('button', { name: 'Resume setup' })).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Skip remaining steps' })).toBeNull();
		expect(screen.queryByRole('button', { name: 'Check this server' })).toBeNull();
		expect(screen.queryByText('This server needs a check')).toBeNull();
		expect(
			text(screen.getByText('Setup did not finish').closest<HTMLElement>('[data-slot="alert"]'))
		).toContain('game files are not owned by uid 10000');
	});

	it('offers no game update while the install is outstanding', async () => {
		await open([actions.view, actions.gameUpdate], {
			row: instance({ state: 'error' }),
			operation: {
				id: 'op-1',
				kind: 'create',
				state: 'interrupted',
				cursor: 0,
				steps: [{ kind: 'provision' }, { kind: 'start' }],
				created_at: '2026-10-10T08:00:00Z',
				updated_at: '2026-10-10T08:01:00Z'
			}
		});
		await screen.findByText('Setup did not finish');

		expect(screen.queryByRole('button', { name: 'Reinstall' })).toBeNull();
		expect(screen.queryByText(/current public build/)).toBeNull();
		expect(daemon.requests('GET', '/instances/inst-a/update-status')).toHaveLength(0);
	});

	it('shows a parked server to a viewer without offering the check', async () => {
		await open([actions.view], { row: instance({ state: 'error' }) });

		expect(screen.getByText('This server needs a check')).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Check this server' })).toBeNull();
	});

	it('says what the failed job reported on a parked server', async () => {
		await open([actions.view], {
			row: instance({ state: 'error' }),
			history: [job({ kind: 'start', status: 'failed', error: 'container exited with code 1' })]
		});

		expect(text(screen.getByRole('alert'))).toContain('container exited with code 1');
	});

	it('raises one red alert at a time', async () => {
		await open([actions.view, actions.start], {
			row: instance({ state: 'error' }),
			history: [
				job({ job_id: 'job-2', kind: 'start', status: 'failed' }),
				job({ job_id: 'job-1', kind: 'stop', status: 'succeeded', clean: false })
			],
			operation: {
				id: 'op-1',
				kind: 'create',
				state: 'interrupted',
				cursor: 2,
				steps: [{ kind: 'provision' }, { kind: 'mod_install' }, { kind: 'start' }],
				created_at: '2026-10-10T08:00:00Z',
				updated_at: '2026-10-10T08:01:00Z'
			}
		});
		await screen.findByText('Setup did not finish');

		const red = screen
			.getAllByRole('alert')
			.filter((alert) => alert.classList.contains('text-destructive'));
		expect(red.map((alert) => text(alert))).toEqual([
			expect.stringContaining('This server needs a check')
		]);
		expect(screen.queryByText('The last stop was not confirmed')).toBeNull();
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

	it('refetches the notices only when the id or state they depend on changes', async () => {
		await open([actions.view, actions.consoleRead], { row: instance({ state: 'running' }) });
		const reads = () =>
			[
				'/instances/inst-a/diagnosis',
				'/instances/inst-a/update-status',
				'/schedules',
				'/instances/inst-a/jobs'
			].map((path) => daemon.requests('GET', path).length);
		await vi.waitFor(() => expect(reads()).toEqual([1, 1, 1, 1]));

		for (let i = 0; i < 10; i++) {
			push('state', { type: 'join_code', instance: 'inst-a', code: `${i}` });
		}
		push('state', {
			type: 'state',
			instance: 'inst-a',
			state: 'running',
			restart_required: false,
			pending_restart: false
		});
		await vi.waitFor(() => expect(reads()).toEqual([1, 1, 1, 2]));

		push('state', {
			type: 'state',
			instance: 'inst-a',
			state: 'stopping',
			restart_required: false,
			pending_restart: false
		});
		await vi.waitFor(() => expect(reads()).toEqual([2, 1, 1, 3]));
	});

	// A clone copies a world at rest: the link is there for a holder of instance.clone and
	// leads anywhere only while the source is stopped.
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

	// Below its own floor the server stops saving the world without an error, so free
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

describe('the operation history', () => {
	// The question the card exists to answer: which update failed, and why.
	it('lists the recent operations and opens a failed one to its error', async () => {
		await open([actions.view], {
			history: [
				job({ job_id: 'job-2', kind: 'backup', status: 'succeeded' }),
				job({
					job_id: 'job-1',
					kind: 'game_update',
					status: 'failed',
					error: 'download stalled',
					error_code: 'steam_unreachable'
				})
			]
		});

		expect(screen.getByText('Backup')).toBeTruthy();
		const failed = screen.getByText('Game update').closest('li');
		expect(text(failed)).toContain('Failed');
		expect(text(failed)).toContain('download stalled');
		expect(text(failed)).toContain('Code steam_unreachable');
	});

	it('fetches older operations by cursor, and stops offering them at the end', async () => {
		await open([actions.view], {
			history: [job({ job_id: 'job-2', kind: 'backup', status: 'succeeded' })],
			next: 'cursor-1'
		});
		daemon.on('GET', '/instances/inst-a/jobs', () =>
			Response.json({
				items: [job({ job_id: 'job-1', kind: 'game_update', status: 'failed' })],
				next_cursor: null
			})
		);

		await click(screen.getByRole('button', { name: 'Show older operations' }));

		expect(await screen.findByText('Game update')).toBeTruthy();
		expect(screen.getByText('Backup')).toBeTruthy();
		const sent = daemon.requests('GET', '/instances/inst-a/jobs').at(-1);
		expect(sent?.query.get('cursor')).toBe('cursor-1');
		expect(screen.queryByRole('button', { name: 'Show older operations' })).toBeNull();
	});

	it('keeps older operations on screen when a state change re-reads the newest page', async () => {
		const newest = Array.from({ length: 10 }, (_, i) =>
			job({ job_id: `job-${20 - i}`, kind: 'backup', status: 'succeeded' })
		);
		await open([actions.view], { history: newest, next: 'cursor-1' });
		daemon.on('GET', '/instances/inst-a/jobs', () =>
			Response.json({
				items: [job({ job_id: 'job-1', kind: 'game_update', status: 'failed' })],
				next_cursor: null
			})
		);
		await click(screen.getByRole('button', { name: 'Show older operations' }));
		expect(await screen.findByText('Game update')).toBeTruthy();

		daemon.on('GET', '/instances/inst-a/jobs', () =>
			Response.json({
				items: [
					job({ job_id: 'job-21', kind: 'restart', status: 'succeeded' }),
					...newest.slice(0, 9)
				],
				next_cursor: 'cursor-2'
			})
		);
		push('state', {
			type: 'state',
			instance: 'inst-a',
			state: 'running',
			restart_required: false,
			pending_restart: false
		});

		expect(await screen.findByText('Restart')).toBeTruthy();
		expect(screen.getByText('Game update')).toBeTruthy();
	});

	it('keeps the resources card short while the server is not running', async () => {
		await open([actions.view, actions.statsRead], { row: instance({ state: 'stopped' }) });

		expect(await screen.findByText('Free')).toBeTruthy();
		expect(
			screen.getByText('CPU, memory and players show here while the server runs.')
		).toBeTruthy();
		expect(screen.queryByText('CPU')).toBeNull();
		expect(screen.queryByText('Players')).toBeNull();
	});

	// Older pages sit in the same list, so an old unconfirmed stop must not read as the last one.
	it('warns of an unconfirmed stop only when it is the newest stop on record', async () => {
		const stop = (job_id: string, clean: boolean) =>
			job({ job_id, kind: 'stop', status: 'succeeded', clean });
		await open([actions.view], { history: [stop('job-2', true), stop('job-1', false)] });
		expect(screen.queryByText('The last stop was not confirmed')).toBeNull();
	});

	it('warns when the newest stop was not confirmed', async () => {
		const stop = (job_id: string, clean: boolean) =>
			job({ job_id, kind: 'stop', status: 'succeeded', clean });
		await open([actions.view], { history: [stop('job-2', false), stop('job-1', true)] });
		expect(screen.getByText('The last stop was not confirmed')).toBeTruthy();
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

	it('says so while there is no output yet', async () => {
		await open(console);
		expect(
			await screen.findByText('No output yet. The server log shows here once it starts.')
		).toBeTruthy();
	});

	it('is not shown without console.read', async () => {
		await open([actions.view]);
		expect(screen.queryByLabelText('Server command')).toBeNull();
	});
});
