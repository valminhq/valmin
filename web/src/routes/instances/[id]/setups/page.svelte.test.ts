import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import type { ManifestLaunch } from '#lib/api/manifest.js';
import type { SetupDetail, SetupPreview } from '#lib/api/setups.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, backup, envelope, instance, job, permissions } from '#lib/testing/daemon.js';
import { socket } from '#lib/testing/socket.js';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/setups') }
}));

vi.mock('$app/state', () => app);
vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

function launch(overrides: Partial<ManifestLaunch> = {}): ManifestLaunch {
	return {
		server_name: 'My Server',
		world_name: 'MyWorld',
		public: false,
		crossplay: false,
		mem_limit_mb: 4096,
		cpu_limit: null,
		backup_keep_cold: 7,
		backup_keep_hot: 3,
		backup_on_restart: false,
		...overrides
	};
}

const saved: SetupDetail = {
	id: 'setup-1',
	name: 'Working before update',
	created_at: '2026-09-28T12:00:00Z',
	created_by: 'sam',
	game_build_id: '100',
	world_name: 'OldWorld',
	world_backup: {
		id: 'bk-1',
		world_name: 'OldWorld',
		created_at: '2026-09-28T11:00:00Z',
		consistent: true
	},
	instance: launch({ world_name: 'OldWorld', backup_keep_cold: 5 }),
	mods: [
		{
			full_name: 'Author-Plugin',
			source: 'thunderstore',
			version: '1.0.0',
			side: 'client_required',
			enabled: false,
			locked: true,
			installed_as: 'explicit'
		}
	],
	configs: [{ file: 'plugin.cfg', content: 'enabled = false\n', sha256: 'saved', size_bytes: 16 }],
	config_tree: true,
	configs_truncated: false,
	automatic: false,
	light: false,
	world_pending: false
};

function preview(overrides: Partial<SetupPreview> = {}): SetupPreview {
	return {
		etag: '"preview-fingerprint"',
		ready: true,
		stop_required: false,
		problems: [],
		current: {
			build: '200',
			launch: launch(),
			mods: [
				{
					full_name: 'Author-Plugin',
					source: 'hexium',
					version: '2.0.0',
					side: 'server_only',
					enabled: true,
					locked: false,
					installed_as: 'explicit'
				}
			],
			configs: [
				{ file: 'plugin.cfg', content: 'enabled = true\n', sha256: 'current', size_bytes: 15 }
			]
		},
		setup: saved,
		...overrides
	};
}

let daemon: FakeDaemon;

function serve({
	state = 'stopped',
	result = preview()
}: { state?: string; result?: SetupPreview } = {}) {
	daemon.on('GET', '/instances/inst-a', () =>
		Response.json(instance({ id: 'inst-a', name: 'Live', state }))
	);
	daemon.on('GET', '/instances/inst-a/setups', () => Response.json({ items: [saved] }));
	daemon.on('GET', '/instances/inst-a/backups', () =>
		Response.json({
			items: [backup(), backup({ id: 'bk-hot', consistent: false })],
			next_cursor: null
		})
	);
	daemon.on('GET', '/instances/inst-a/setups/setup-1/preview', () => Response.json(result));
}

/** The card under the title text. */
function card(title: string): HTMLElement {
	return screen.getByText(title).closest('[data-slot="card"]') as HTMLElement;
}

async function open() {
	render(Page);
	await screen.findByRole('heading', { name: 'Saved for this server' });
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.permissions = permissions('inst-a', [actions.setupsManage]);
});

afterEach(() => {
	session.permissions = null;
	socket.reset();
	vi.unstubAllGlobals();
});

describe('Saved setups', () => {
	it('does not load setup data without the administrator action', async () => {
		session.permissions = permissions('inst-a', [actions.view]);
		render(Page);
		expect(screen.getByText("You don't have permission to manage saved setups.")).toBeTruthy();
		expect(daemon.requests('GET', '/instances/inst-a/setups')).toHaveLength(0);
	});

	it('saves a named setup with only a consistent selected world backup', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		const selector = screen.getByLabelText('Associated world backup') as HTMLSelectElement;
		await vi.waitFor(() => expect(selector.options).toHaveLength(3));
		expect([...selector.options].map((option) => option.value)).toEqual(['next', '', 'bk-1']);
		expect((screen.getByLabelText('Name') as HTMLInputElement).value).toBe('Working before update');
		await fireEvent.change(selector, { target: { value: 'bk-1' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Save setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/setups')[0].body).toEqual({
			name: 'Working before update',
			world_backup_id: 'bk-1'
		});
	});

	it('backs up the world now and links it when chosen', async () => {
		session.permissions = permissions('inst-a', [actions.setupsManage, actions.backupsCreate]);
		serve();
		daemon.on('POST', '/instances/inst-a/setups', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		const selector = screen.getByLabelText('Associated world backup') as HTMLSelectElement;
		await vi.waitFor(() => expect(selector.options).toHaveLength(4));
		await fireEvent.change(selector, { target: { value: 'now' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Save setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/setups')[0].body).toEqual({
			name: 'Working before update',
			back_up_world: true
		});
	});

	it('saves a light setup when package files are not kept', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		const saveCard = card('Save current setup');
		await fireEvent.click(within(saveCard).getByRole('switch', { name: /Keep package files/ }));
		expect(
			within(saveCard).getByText(/It fails for a version the registry has removed/)
		).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Save setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/setups')[0].body).toEqual({
			name: 'Working before update',
			link_next_world_backup: true,
			light: true
		});
	});

	it('saves while running, waiting for the next update to link a world backup', async () => {
		session.permissions = permissions('inst-a', [actions.setupsManage, actions.backupsCreate]);
		serve({ state: 'running' });
		daemon.on('POST', '/instances/inst-a/setups', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		const selector = screen.getByLabelText('Associated world backup') as HTMLSelectElement;
		await vi.waitFor(() => expect(selector.options).toHaveLength(3));
		expect([...selector.options].map((option) => option.value)).not.toContain('now');
		await fireEvent.click(screen.getByRole('button', { name: 'Save setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/setups')[0].body).toEqual({
			name: 'Working before update',
			link_next_world_backup: true
		});
	});

	it('says a setup waits for the next update to link a world backup', async () => {
		serve();
		daemon.on('GET', '/instances/inst-a/setups', () =>
			Response.json({ items: [{ ...saved, world_backup: null, world_pending: true }] })
		);
		await open();
		expect(card('Saved for this server').textContent).toContain(
			'World backup: from the next mod update, if the mods are still the same'
		);
	});

	it('saves what mod changes save first from the two switches', async () => {
		serve();
		daemon.on('PATCH', '/instances/inst-a', () =>
			Response.json(instance({ id: 'inst-a', name: 'Live', setup_before_update: 'light' }))
		);
		await open();
		const before = card('Before mod changes');
		const save = within(before).getByRole('button', { name: 'Save' }) as HTMLButtonElement;
		expect(save.disabled).toBe(true);

		await fireEvent.click(within(before).getByRole('switch', { name: /Keep package files/ }));
		expect(before.textContent).toContain('It fails for a version the registry has removed.');
		await fireEvent.click(save);
		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({
			setup_before_update: 'light'
		});
		await vi.waitFor(() => expect(save.disabled).toBe(true));

		await fireEvent.click(
			within(before).getByRole('switch', { name: 'Save the setup before mod changes' })
		);
		expect(within(before).queryByRole('switch', { name: /Keep package files/ })).toBeNull();
		await fireEvent.click(save);
		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(2));
		expect(daemon.requests('PATCH', '/instances/inst-a')[1].body).toEqual({
			setup_before_update: 'off'
		});
	});

	it('saves how many automatic setups and world backups are kept', async () => {
		serve();
		daemon.on('PATCH', '/instances/inst-a', () =>
			Response.json(instance({ id: 'inst-a', name: 'Live', setup_versions_kept: 5 }))
		);
		await open();
		const before = card('Before mod changes');
		const save = within(before).getByRole('button', { name: 'Save' }) as HTMLButtonElement;
		const versions = within(before).getByLabelText('Automatic setups to keep');

		await fireEvent.input(versions, { target: { value: '0' } });
		expect(before.textContent).toContain('Enter a whole number from 1 to 100.');
		expect(save.disabled).toBe(true);

		await fireEvent.input(versions, { target: { value: '5' } });
		await fireEvent.click(save);
		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({
			setup_versions_kept: 5
		});
	});

	it('says who saved each setup', async () => {
		serve();
		await open();
		expect(card('Saved for this server').textContent).toContain('by sam');
	});

	it('marks automatic setups, setups saved before full capture, and left-out files', async () => {
		serve();
		daemon.on('GET', '/instances/inst-a/setups', () =>
			Response.json({
				items: [
					{ ...saved, id: 'auto', name: 'Before restoring x', automatic: true, light: true },
					{ ...saved, id: 'old', name: 'Old', config_tree: false, configs_truncated: true }
				]
			})
		);
		await open();
		expect(screen.getByText('Automatic')).toBeTruthy();
		expect(screen.getByText('Light')).toBeTruthy();
		expect(screen.getByText('Top-level .cfg only')).toBeTruthy();
		expect(screen.getByText(/Some config files are not included/)).toBeTruthy();
	});

	it('lists removed and added config files and loads a changed file only when opened', async () => {
		const file = (name: string, sha256: string) => ({ file: name, sha256, size_bytes: 1 });
		serve({
			result: preview({
				current: {
					...preview().current,
					configs: [file('EWD/a.yaml', 'now'), file('EWD/new.yaml', 'n')]
				},
				setup: { ...saved, configs: [file('EWD/a.yaml', 'then'), file('old.json', 'o')] }
			})
		});
		daemon.on('GET', '/instances/inst-a/configs/EWD%2Fa.yaml/raw', () => new Response('a: 2\n'));
		daemon.on(
			'GET',
			'/instances/inst-a/setups/setup-1/configs/EWD%2Fa.yaml',
			() => new Response('a: 1\n')
		);
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Removed by the restore (1)' });
		expect(screen.getByText('EWD/new.yaml')).toBeTruthy();
		expect(screen.getByRole('heading', { name: 'Added by the restore (1)' })).toBeTruthy();
		expect(screen.getByText('old.json')).toBeTruthy();
		expect(
			daemon.requests('GET', '/instances/inst-a/setups/setup-1/configs/EWD%2Fa.yaml')
		).toHaveLength(0);

		const details = screen.getByText('EWD/a.yaml').closest('details')!;
		details.open = true;
		await fireEvent(details, new Event('toggle'));
		await vi.waitFor(() => expect(within(details).getByText(/a: 1/)).toBeTruthy());
		expect(within(details).getByText(/a: 2/)).toBeTruthy();
	});

	it('rolls a running server back, stopping it first and starting it again', async () => {
		session.permissions = permissions('inst-a', [
			actions.setupsManage,
			actions.start,
			actions.stop,
			actions.backupsRestore
		]);
		serve({ state: 'running', result: preview({ stop_required: true }) });
		daemon.on('POST', '/instances/inst-a/setups/setup-1/rollback', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		expect(screen.getByText(/so it is stopped first and players are disconnected/)).toBeTruthy();
		await fireEvent.click(
			screen.getByRole('switch', { name: /Also restore the linked world backup/ })
		);
		await fireEvent.click(screen.getByRole('button', { name: 'Roll back to this setup' }));
		const dialog = screen.getByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/Type Live to confirm/), {
			target: { value: 'Live' }
		});
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Roll back' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')).toHaveLength(1)
		);
		const request = daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')[0];
		expect(request.body).toEqual({ world: true, start: true });
		expect(request.headers['If-Match']).toBeUndefined();
	});

	it('asks for the server name only when the rollback restores the world', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups/setup-1/rollback', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		await fireEvent.click(screen.getByRole('button', { name: 'Roll back to this setup' }));
		const dialog = screen.getByRole('dialog');
		expect(within(dialog).queryByLabelText(/Type Live to confirm/)).toBeNull();
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Roll back' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')[0].body).toEqual(
			{}
		);
	});

	it('keeps rollback unavailable while the server is starting', async () => {
		serve({ state: 'starting' });
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		expect(
			screen.getByRole('button', { name: 'Roll back to this setup' }).hasAttribute('disabled')
		).toBe(true);
		expect(
			screen.getAllByText('Wait for this server to finish starting or stopping.')
		).toHaveLength(2);
	});

	it('says a server in error needs a check before setups can be saved or rolled back', async () => {
		serve({ state: 'error' });
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		expect(screen.getAllByText('This server needs a check first; open its overview.')).toHaveLength(
			2
		);
	});

	it('compares exact mod state, shows comparison-only fields, and rolls back with the preview fingerprint', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups/setup-1/rollback', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		const rows = screen.getAllByRole('rowheader');
		expect(rows.map((row) => row.textContent?.trim())).toContain('Game build Compare only');
		expect(rows.map((row) => row.textContent?.trim())).toContain('World name Compare only');
		expect(screen.getByText('Different version lock')).toBeTruthy();
		expect(screen.getByText('Different client tag')).toBeTruthy();
		expect(screen.getByText('Different registry')).toBeTruthy();
		expect(screen.getByText('plugin.cfg')).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Roll back to this setup' }));
		await fireEvent.click(
			within(screen.getByRole('dialog')).getByRole('button', { name: 'Roll back' })
		);
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')).toHaveLength(1)
		);
		expect(
			daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')[0].headers['If-Match']
		).toBe('"preview-fingerprint"');
	});

	it('identifies an unavailable linked world backup without blocking setup restore', async () => {
		serve({
			result: preview({
				setup: { ...saved, world_backup: { ...saved.world_backup!, available: false } }
			})
		});
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByText('The linked world backup is unavailable.');
		expect(
			screen.getByRole('button', { name: 'Roll back to this setup' }).hasAttribute('disabled')
		).toBe(false);
	});

	it('deletes the selected setup through a job after confirmation', async () => {
		serve();
		daemon.on('DELETE', '/instances/inst-a/setups/setup-1', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		await fireEvent.click(screen.getByRole('button', { name: 'Delete setup' }));
		const dialog = screen.getByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/Type Live to confirm/), {
			target: { value: 'Live' }
		});
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Delete setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', '/instances/inst-a/setups/setup-1')).toHaveLength(1)
		);
		expect(daemon.requests('DELETE', '/instances/inst-a/backups/bk-1')).toHaveLength(0);
	});

	// The reload after a job keeps the page in place instead of flashing it back to "Loading…".
	it('keeps the page on screen while the list is re-read after a delete', async () => {
		serve();
		daemon.on('DELETE', '/instances/inst-a/setups/setup-1', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ status: 'succeeded' })));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		await fireEvent.click(screen.getByRole('button', { name: 'Delete setup' }));
		const dialog = screen.getByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/Type Live to confirm/), {
			target: { value: 'Live' }
		});
		daemon.on('GET', '/instances/inst-a/setups', () => new Promise<Response>(() => {}));
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Delete setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/instances/inst-a/setups')).toHaveLength(2)
		);

		expect(screen.queryByText('Loading…')).toBeNull();
		expect(screen.getByRole('heading', { name: 'Saved for this server' })).toBeTruthy();
	});

	it('blocks an unready preview and shows the reason', async () => {
		serve({ result: preview({ ready: false, problems: ['Package payload is missing.'] }) });
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByText('Package payload is missing.');
		expect(
			screen.getByRole('button', { name: 'Roll back to this setup' }).hasAttribute('disabled')
		).toBe(true);
	});

	it('invalidates a stale preview instead of retrying a restore', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups/setup-1/rollback', () =>
			envelope(412, 'stale_write', 'The server changed since this preview.')
		);
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Rollback preview' });
		await fireEvent.click(screen.getByRole('button', { name: 'Roll back to this setup' }));
		await fireEvent.click(
			within(screen.getByRole('dialog')).getByRole('button', { name: 'Roll back' })
		);
		await screen.findByText('The server changed since this preview.');
		expect(screen.queryByRole('heading', { name: 'Rollback preview' })).toBeNull();
		expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/rollback')).toHaveLength(1);
	});
});
