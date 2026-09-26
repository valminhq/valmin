import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Backup } from '$lib/api/backups';
import { actions, type Instance, type WorldOnDisk } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { click } from '$lib/testing/interact';
import { FakeDaemon, backup, instance, job, permissions } from '$lib/testing/daemon';
import BackupsPanel from './backups-panel.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const list = '/instances/inst-a/backups';
let daemon: FakeDaemon;

function world(overrides: Partial<WorldOnDisk> = {}): WorldOnDisk {
	return {
		name: 'MyWorld',
		dir: 'worlds_local',
		layout: 'directory',
		bytes: 40 * 1024 * 1024,
		db_bytes: 1024,
		fwl_bytes: 64,
		modified_at: '2026-09-20T12:00:00Z',
		loaded: true,
		complete: true,
		...overrides
	};
}

/** Renders the panel for a member holding `held`, once the catalogue's first read settles. */
async function open(
	held: string[],
	{
		archives = [backup()],
		row = instance(),
		worlds = [world()]
	}: { archives?: Backup[]; row?: Instance; worlds?: WorldOnDisk[] } = {}
) {
	daemon.on('GET', list, () => Response.json({ items: archives, next_cursor: null }));
	daemon.on('GET', '/instances/inst-a/worlds', () =>
		Response.json({ items: worlds, next_cursor: null })
	);
	session.permissions = permissions('inst-a', held);
	const view = render(BackupsPanel, { instance: row });
	if (held.includes(actions.backupsList)) await screen.findByText('Latest backup');
	return view;
}

const button = (name: string | RegExp) => screen.getByRole('button', { name });
const disabled = (el: HTMLElement) => (el as HTMLButtonElement).disabled;

/** Types `name` into the open confirmation and presses its confirm button. */
async function confirmTyping(name: string, confirmLabel: string) {
	const dialog = await screen.findByRole('dialog');
	const confirm = within(dialog).getByRole('button', { name: confirmLabel });
	expect(disabled(confirm), 'nothing confirms before the name is typed').toBe(true);
	await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
		target: { value: name }
	});
	await click(confirm);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the backups panel', () => {
	// F3. Four capabilities, each gating its own control, and retention is instance.settings
	// rather than any of them (ADR-121, ADR-126).
	it('shows a list-only member the catalogue and no control', async () => {
		await open([actions.backupsList]);

		expect(screen.getByText('Consistent')).toBeTruthy();
		for (const name of ['Stop and back up', 'Back up without stopping', 'Restore', 'Delete']) {
			expect(screen.queryByRole('button', { name }), name).toBeNull();
		}
		expect(screen.queryByRole('link', { name: 'Download' })).toBeNull();
		expect(screen.queryByText('Backup settings')).toBeNull();
	});

	it('tells a member who cannot list backups so, and shows no catalogue', async () => {
		await open([actions.view]);

		expect(screen.getByText('Backups are not available to you.')).toBeTruthy();
		expect(screen.queryByText('Latest backup')).toBeNull();
		expect(screen.queryByText('Worlds on disk')).toBeNull();
	});

	it('gives each capability its own control', async () => {
		await open([
			actions.backupsList,
			actions.backupsCreate,
			actions.backupsDownload,
			actions.backupsRestore,
			actions.settings
		]);

		expect(button('Stop and back up')).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Download' }).getAttribute('href')).toBe(
			'/api/v1/instances/inst-a/backups/bk-1/download'
		);
		expect(button('Restore')).toBeTruthy();
		expect(button('Delete')).toBeTruthy();
		expect(screen.getByLabelText('Backups to keep (server stopped)')).toBeTruthy();
	});

	// 12 §3.2 and B12. The two controls are not two speeds of one thing: a quiesced archive
	// stops the server, which is what makes it trustworthy; a hot copy may not restore.
	it('sends the mode the operator chose, and says what each costs', async () => {
		await open([actions.backupsList, actions.backupsCreate]);
		daemon.on('POST', list, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		expect(screen.getByText(/offline for the whole backup/)).toBeTruthy();
		expect(
			screen.getByText(/may contain an incomplete save and may not be restorable/)
		).toBeTruthy();

		await click(button('Stop and back up'));
		await vi.waitFor(() => expect(daemon.requests('POST', list)).toHaveLength(1));
		expect(daemon.requests('POST', list)[0].query.get('mode')).toBe('quiesced');
	});

	it('sends hot for the best-effort copy', async () => {
		await open([actions.backupsList, actions.backupsCreate]);
		daemon.on('POST', list, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(button('Back up without stopping'));
		await vi.waitFor(() => expect(daemon.requests('POST', list)).toHaveLength(1));
		expect(daemon.requests('POST', list)[0].query.get('mode')).toBe('hot');
	});

	// B12 at the point of choosing: a row that says nothing about consistency reads as
	// trustworthy, and a hot copy is not.
	it('labels every archive by the kind of copy it is', async () => {
		await open([actions.backupsList], {
			archives: [
				backup({ id: 'hot', consistent: false }),
				backup({ id: 'cold', consistent: true, created_at: '2026-09-19T12:00:00Z' })
			]
		});

		const rows = screen.getAllByRole('listitem').filter((li) => li.textContent?.includes('Size'));
		expect(rows[0].textContent).toContain('Best-effort');
		expect(rows[0].textContent).toContain('copied while running');
		expect(rows[1].textContent).toContain('Consistent');
		expect(rows[1].textContent).toContain('server was stopped');
	});

	it('states the latest recovery point, or that there is none', async () => {
		await open([actions.backupsList], { archives: [] });
		expect(screen.getByText('Nothing has been backed up yet')).toBeTruthy();
	});

	// The daemon decides which archives the next prune removes, over the whole catalogue;
	// the screen marks exactly those.
	it('marks the archives the daemon says the next prune removes, and only those', async () => {
		await open([actions.backupsList], {
			archives: [backup({ id: 'kept' }), backup({ id: 'doomed', prunes_next: true })]
		});

		expect(screen.getAllByText('Pending deletion')).toHaveLength(1);
		const rows = screen.getAllByRole('listitem').filter((li) => li.textContent?.includes('Size'));
		expect(rows[1].textContent).toContain('Pending deletion');
	});

	// F5. A restore replaces the world this server loads, so the world's name is typed back
	// and nothing is sent until it is.
	it('restores the chosen archive only after the world name is typed back', async () => {
		await open([actions.backupsList, actions.backupsRestore], {
			archives: [backup({ id: 'newest' }), backup({ id: 'older' })]
		});
		daemon.on('POST', '/instances/inst-a/backups/older/restore', () =>
			Response.json(job(), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(screen.getAllByRole('button', { name: 'Restore' })[1]);
		expect((await screen.findByRole('dialog')).textContent).toContain(
			'Restore backup for MyWorld?'
		);
		await confirmTyping('MyWorld', 'Restore backup');

		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/backups/older/restore')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/backups/newest/restore')).toHaveLength(0);
	});

	it('refuses a restore into a running server, and says why', async () => {
		await open([actions.backupsList, actions.backupsRestore], {
			row: instance({ state: 'running' })
		});

		expect(disabled(button('Restore'))).toBe(true);
		expect(screen.getByText('Stop this server to restore a world into it.')).toBeTruthy();
	});

	it('deletes an archive after naming it and typing the server back, then re-reads', async () => {
		await open([actions.backupsList, actions.backupsRestore]);
		daemon.on(
			'DELETE',
			'/instances/inst-a/backups/bk-1',
			() => new Response(null, { status: 204 })
		);

		await click(button('Delete'));
		expect((await screen.findByRole('dialog')).textContent).toContain(backup().filename);
		await confirmTyping('inst-a', 'Delete backup');

		await vi.waitFor(() => expect(daemon.requests('GET', list)).toHaveLength(2));
		expect(daemon.requests('DELETE', '/instances/inst-a/backups/bk-1')).toHaveLength(1);
	});

	// F4. A backup is the daemon's job, and the catalogue shown afterwards is the daemon's —
	// not a row the panel added because it expected one.
	it('follows the job and re-reads the catalogue only once it has finished', async () => {
		await open([actions.backupsList, actions.backupsCreate], { archives: [] });
		let status: 'running' | 'succeeded' = 'running';
		daemon.on('POST', list, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () =>
			Response.json(job({ status, progress: 40, message: 'Copying the world' }))
		);

		await click(button('Stop and back up'));
		expect(await screen.findByText(/Copying the world/)).toBeTruthy();
		expect(daemon.requests('GET', list), 'nothing re-read while it runs').toHaveLength(1);
		expect(screen.getByText('Nothing has been backed up yet'), 'and no row predicted').toBeTruthy();

		const { socket } = await import('$lib/testing/socket');
		status = 'succeeded';
		socket.push('job.job-1', {
			type: 'job',
			id: 'job-1',
			kind: 'backup',
			status: 'succeeded',
			progress: 100,
			message: 'Backup complete'
		});
		await vi.waitFor(() => expect(daemon.requests('GET', list)).toHaveLength(2));
	});

	// The two counts are separate because the classes are: one budget lets a burst of hot
	// copies evict every quiesced archive.
	it('saves retention as two counts and the restart switch', async () => {
		await open([actions.backupsList, actions.settings]);
		daemon.on('PATCH', '/instances/inst-a', () =>
			Response.json(instance({ backup_keep_cold: 10, backup_keep_hot: 2, backup_on_restart: true }))
		);

		expect(screen.getByText(/The restart waits for the backup to finish/)).toBeTruthy();
		await fireEvent.input(screen.getByLabelText('Backups to keep (server stopped)'), {
			target: { value: '10' }
		});
		await fireEvent.input(screen.getByLabelText('Backups to keep (best-effort)'), {
			target: { value: '2' }
		});
		await click(screen.getByRole('switch', { name: 'Back up when this server restarts' }));
		await click(button('Save backup settings'));

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({
			backup_keep_cold: 10,
			backup_keep_hot: 2,
			backup_on_restart: true
		});
	});

	it('will not save an emptied retention count', async () => {
		await open([actions.backupsList, actions.settings]);

		await fireEvent.input(screen.getByLabelText('Backups to keep (server stopped)'), {
			target: { value: '' }
		});
		expect(disabled(button('Save backup settings'))).toBe(true);
	});

	it('will not save a negative retention count', async () => {
		await open([actions.backupsList, actions.settings]);

		await fireEvent.input(screen.getByLabelText('Backups to keep (best-effort)'), {
			target: { value: '-1' }
		});
		expect(disabled(button('Save backup settings'))).toBe(true);
		expect(screen.getByText('Enter zero or a positive whole number.')).toBeTruthy();
	});

	// An older recovery point the panel cannot reach is one the operator does not have.
	it('appends older pages rather than replacing what is on screen', async () => {
		await open([actions.backupsList]);
		daemon.on('GET', list, (r) =>
			r.query.get('cursor') === 'page-2'
				? Response.json({
						items: [backup({ id: 'old', world_name: 'OldWorld' })],
						next_cursor: null
					})
				: Response.json({
						items: [backup({ id: 'new', world_name: 'NewWorld' })],
						next_cursor: 'page-2'
					})
		);
		// Re-render against the paged catalogue.
		document.body.innerHTML = '';
		render(BackupsPanel, { instance: instance() });
		await screen.findByText('NewWorld');

		await click(button('Load older backups'));
		await screen.findByText('OldWorld');
		expect(screen.getByText('NewWorld'), 'the first page is still there').toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Load older backups' })).toBeNull();
	});
});

describe('worlds on disk', () => {
	// A backup refuses when the savedir lacks the world this server loads. Said once here,
	// beside the catalogue, rather than after each failed attempt.
	it('says when the world this server loads is not on disk', async () => {
		await open([actions.backupsList], { worlds: [world({ name: 'Other', loaded: false })] });

		expect(await screen.findByText('This server’s world is not here')).toBeTruthy();
	});

	it('says nothing is missing when the loaded world is there', async () => {
		await open([actions.backupsList]);

		await screen.findByText('Worlds on disk');
		expect(screen.queryByText('This server’s world is not here')).toBeNull();
	});

	// 03 §4: a 1.0 world's .db2 is a small part of it; the size shown is the whole world.
	it('shows the whole world’s size, not its database file', async () => {
		await open([actions.backupsList], {
			worlds: [world({ bytes: 40 * 1024 * 1024, db_bytes: 1024 })]
		});

		expect(await screen.findByText(/40\.0 MB/)).toBeTruthy();
	});

	// A listing that could not be read is not an empty savedir, and not a missing world.
	it('reports a failed read instead of claiming the world is missing', async () => {
		await open([actions.backupsList]);
		daemon.on('GET', '/instances/inst-a/worlds', () =>
			Response.json({ error: { code: 'internal', message: 'x', request_id: 'r' } }, { status: 500 })
		);
		document.body.innerHTML = '';
		render(BackupsPanel, { instance: instance() });

		const retry = await screen.findByRole('button', { name: 'Retry reading the save directory' });
		expect(screen.queryByText('This server’s world is not here')).toBeNull();
		expect(screen.queryByText(/Nothing in this server’s save directory yet/)).toBeNull();

		daemon.on('GET', '/instances/inst-a/worlds', () =>
			Response.json({ items: [world()], next_cursor: null })
		);
		await click(retry);
		expect(await screen.findByText('· loaded by this server')).toBeTruthy();
	});
});
