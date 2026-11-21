import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { click } from '#lib/testing/interact.js';
import { FakeDaemon, backup, instance, job } from '#lib/testing/daemon.js';
import WorldImportBackup from './world-import-backup.svelte';

vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

const target = '/instances/inst-a/worlds/import-backup';
let daemon: FakeDaemon;

/** What the signed-in user holds: import on the target, download on the other server, and
 * the administrator's adopt action when admin is set. */
function open(opts: { admin?: boolean; download?: boolean } = {}, row: Instance = instance()) {
	session.permissions = {
		user_id: 'u-test',
		role: 'member',
		allowed_actions: opts.admin ? [actions.adopt] : [],
		instances: [
			{ instance_id: 'inst-a', allowed_actions: [actions.worldImport] },
			{
				instance_id: 'inst-b',
				allowed_actions: opts.download === false ? [] : [actions.backupsDownload]
			}
		]
	};
	render(WorldImportBackup, { instance: row });
}

async function choose(label: string, value: string) {
	const select = (await screen.findByLabelText(label)) as HTMLSelectElement;
	await fireEvent.change(select, { target: { value } });
}

async function confirmImport(world = 'MyWorld') {
	await click(screen.getByRole('button', { name: 'Import backup' }));
	const dialog = await screen.findByRole('dialog');
	await fireEvent.input(within(dialog).getByLabelText(/to confirm/), { target: { value: world } });
	await click(within(dialog).getByRole('button', { name: 'Import backup' }));
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: [instance(), instance({ id: 'inst-b', name: 'Valheim B' })],
			next_cursor: null
		})
	);
	daemon.on('GET', '/instances/inst-b/backups', () =>
		Response.json({
			items: [backup({ id: 'bk-9', instance_id: 'inst-b', world_name: 'Dvalin' })],
			next_cursor: null
		})
	);
	daemon.on('GET', '/backups/orphaned', () =>
		Response.json({
			items: [
				{
					instance_id: 'gone-1',
					filename: 'old.tar.gz',
					size_bytes: 2048,
					modified_at: '2026-09-01T00:00:00Z'
				}
			],
			next_cursor: null
		})
	);
	daemon.on('POST', target, () => Response.json(job(), { status: 202 }));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job({ message: 'Archiving the old world' })));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the import from a backup panel', () => {
	it('is closed to someone without world.import', () => {
		session.permissions = {
			user_id: 'u-test',
			role: 'member',
			allowed_actions: [],
			instances: [{ instance_id: 'inst-a', allowed_actions: [] }]
		};
		render(WorldImportBackup, { instance: instance() });
		expect(screen.getByTestId('import-backup-blocked')).toBeTruthy();
	});

	// The other server is offered only when the user may download its backups, and the
	// target itself is never a source.
	it('lists only the servers whose backups the user may download', async () => {
		open({ download: false });
		await waitFor(() => expect(daemon.requests('GET', '/instances')).toHaveLength(1));
		expect(await screen.findByTestId('import-backup-empty')).toBeTruthy();
		expect(screen.queryByRole('option', { name: 'Valheim B' })).toBeNull();
	});

	it('imports a backup of another server once the world name is typed', async () => {
		open();
		await choose('Server', 'live:inst-b');
		await choose('Backup', 'bk-9');
		await confirmImport();

		await waitFor(() => expect(daemon.requests('POST', target)).toHaveLength(1));
		expect(daemon.requests('POST', target)[0].body).toEqual({
			source_instance_id: 'inst-b',
			backup_id: 'bk-9'
		});
		expect(await screen.findByText(/Archiving the old world/)).toBeTruthy();
	});

	// Deleted servers are an administrator's list, and name the archive by filename.
	it('offers the archives of deleted servers to an administrator only', async () => {
		open({ admin: true });
		await screen.findByRole('option', { name: 'Deleted server gone-1' });
		await choose('Server', 'gone:gone-1');
		await choose('Backup', 'old.tar.gz');
		await confirmImport();

		await waitFor(() => expect(daemon.requests('POST', target)).toHaveLength(1));
		expect(daemon.requests('POST', target)[0].body).toEqual({
			source_instance_id: 'gone-1',
			filename: 'old.tar.gz'
		});
	});

	it('does not ask for deleted servers without the adopt action', async () => {
		open();
		await screen.findByLabelText('Server');
		expect(daemon.requests('GET', '/backups/orphaned')).toHaveLength(0);
		expect(screen.queryByRole('group', { name: 'Deleted servers' })).toBeNull();
	});

	// A backup holds every world its server kept. The daemon names them, and the choice is sent
	// back with the same archive.
	it('asks which world to take when the backup holds several', async () => {
		let attempts = 0;
		daemon.on('POST', target, () => {
			attempts += 1;
			if (attempts > 1) return Response.json(job(), { status: 202 });
			return Response.json(
				{
					error: {
						code: 'validation_failed',
						message: 'The backup holds several worlds; name the one to import.',
						request_id: 'req-test',
						details: { worlds: ['Dvalin', 'Midgard'] }
					}
				},
				{ status: 422 }
			);
		});
		open();
		await choose('Server', 'live:inst-b');
		await choose('Backup', 'bk-9');
		await confirmImport();

		await choose('World', 'Midgard');
		await confirmImport();
		await waitFor(() => expect(daemon.requests('POST', target)).toHaveLength(2));
		expect(daemon.requests('POST', target)[1].body).toEqual({
			source_instance_id: 'inst-b',
			backup_id: 'bk-9',
			world: 'Midgard'
		});
	});

	it('holds the import while the server is running', async () => {
		open({}, instance({ state: 'running' }));
		expect(
			await screen.findByText('This server is running. Stop it to import a world.')
		).toBeTruthy();
		expect(
			(screen.getByRole('button', { name: 'Import backup' }) as HTMLButtonElement).disabled
		).toBe(true);
	});

	// A list that arrives after the operator moved on must not fill the other server's choices.
	it('drops a backup list that arrives after another server was chosen', async () => {
		let release: () => void = () => {};
		const held = new Promise<void>((resolve) => (release = resolve));
		daemon.on('GET', '/instances', () =>
			Response.json({
				items: [
					instance(),
					instance({ id: 'inst-b', name: 'Valheim B' }),
					instance({ id: 'inst-c', name: 'Valheim C' })
				],
				next_cursor: null
			})
		);
		daemon.on('GET', '/instances/inst-b/backups', async () => {
			await held;
			return Response.json({
				items: [backup({ id: 'bk-b', instance_id: 'inst-b', world_name: 'Late' })],
				next_cursor: null
			});
		});
		daemon.on('GET', '/instances/inst-c/backups', () =>
			Response.json({ items: [], next_cursor: null })
		);
		session.permissions = {
			user_id: 'u-test',
			role: 'member',
			allowed_actions: [],
			instances: [
				{ instance_id: 'inst-a', allowed_actions: [actions.worldImport] },
				{ instance_id: 'inst-b', allowed_actions: [actions.backupsDownload] },
				{ instance_id: 'inst-c', allowed_actions: [actions.backupsDownload] }
			]
		};
		render(WorldImportBackup, { instance: instance() });
		await screen.findByRole('option', { name: 'Valheim C' });

		await choose('Server', 'live:inst-b');
		await choose('Server', 'live:inst-c');
		await waitFor(() =>
			expect(daemon.requests('GET', '/instances/inst-c/backups')).toHaveLength(1)
		);
		release();
		await new Promise((resolve) => setTimeout(resolve, 0));
		expect(screen.queryByRole('option', { name: /Late/ })).toBeNull();
	});

	// Retention can keep more backups than one page holds, and an older one is still a choice.
	it('offers every page of a server’s backups', async () => {
		daemon.on('GET', '/instances/inst-b/backups', (req) =>
			Response.json(
				req.query.get('cursor')
					? {
							items: [backup({ id: 'bk-old', instance_id: 'inst-b', world_name: 'Oldest' })],
							next_cursor: null
						}
					: {
							items: [backup({ id: 'bk-new', instance_id: 'inst-b', world_name: 'Newest' })],
							next_cursor: 'c2'
						}
			)
		);
		open();
		await choose('Server', 'live:inst-b');
		expect(await screen.findByRole('option', { name: /Oldest/ })).toBeTruthy();
		expect(screen.getByRole('option', { name: /Newest/ })).toBeTruthy();
	});

	// An error belongs to the choice that caused it.
	it('clears an error when another server is chosen', async () => {
		daemon.on('GET', '/instances/inst-b/backups', () =>
			Response.json(
				{ error: { code: 'internal', message: 'Listing failed.', request_id: 'req-test' } },
				{ status: 500 }
			)
		);
		open({ admin: true });
		await screen.findByRole('option', { name: 'Deleted server gone-1' });
		await choose('Server', 'live:inst-b');
		expect(await screen.findByText('Listing failed.')).toBeTruthy();

		await choose('Server', 'gone:gone-1');
		await waitFor(() => expect(screen.queryByText('Listing failed.')).toBeNull());
	});
});
