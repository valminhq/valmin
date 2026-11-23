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
	light: false
};

function preview(overrides: Partial<SetupPreview> = {}): SetupPreview {
	return {
		etag: '"preview-fingerprint"',
		ready: true,
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
		const selector = screen.getByLabelText(
			'Associated world backup (optional)'
		) as HTMLSelectElement;
		await vi.waitFor(() => expect(selector.options).toHaveLength(2));
		expect([...selector.options].map((option) => option.value)).toEqual(['', 'bk-1']);
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
		const selector = screen.getByLabelText(
			'Associated world backup (optional)'
		) as HTMLSelectElement;
		await vi.waitFor(() => expect(selector.options).toHaveLength(3));
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
			light: true
		});
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

	it('keeps save and restore unavailable while the server is running', async () => {
		serve({ state: 'running' });
		await open();
		expect(screen.getByRole('button', { name: 'Save setup' }).hasAttribute('disabled')).toBe(true);
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Restore preview' });
		expect(screen.getByRole('button', { name: 'Restore setup' }).hasAttribute('disabled')).toBe(
			true
		);
		expect(screen.getByText('Stop this server to restore a setup.')).toBeTruthy();
	});

	it('compares exact mod state, shows comparison-only fields, and restores with the preview fingerprint', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups/setup-1/restore', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Restore preview' });
		const rows = screen.getAllByRole('rowheader');
		expect(rows.map((row) => row.textContent?.trim())).toContain('Game build Compare only');
		expect(rows.map((row) => row.textContent?.trim())).toContain('World name Compare only');
		expect(screen.getByText('Different version lock')).toBeTruthy();
		expect(screen.getByText('Different client tag')).toBeTruthy();
		expect(screen.getByText('Different registry')).toBeTruthy();
		expect(screen.getByText('plugin.cfg')).toBeTruthy();
		expect(screen.getByText(/To restore the world too, choose it separately under/)).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Restore setup' }));
		const dialog = screen.getByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/Type Live to confirm/), {
			target: { value: 'Live' }
		});
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Restore setup' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/restore')).toHaveLength(1)
		);
		expect(
			daemon.requests('POST', '/instances/inst-a/setups/setup-1/restore')[0].headers['If-Match']
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
		expect(screen.getByRole('button', { name: 'Restore setup' }).hasAttribute('disabled')).toBe(
			false
		);
	});

	it('deletes the selected setup through a job after confirmation', async () => {
		serve();
		daemon.on('DELETE', '/instances/inst-a/setups/setup-1', () => Response.json(job()));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Restore preview' });
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
		await screen.findByRole('heading', { name: 'Restore preview' });
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
		expect(screen.getByRole('button', { name: 'Restore setup' }).hasAttribute('disabled')).toBe(
			true
		);
	});

	it('invalidates a stale preview instead of retrying a restore', async () => {
		serve();
		daemon.on('POST', '/instances/inst-a/setups/setup-1/restore', () =>
			envelope(412, 'stale_write', 'The server changed since this preview.')
		);
		await open();
		await fireEvent.click(screen.getByRole('button', { name: 'Compare and preview' }));
		await screen.findByRole('heading', { name: 'Restore preview' });
		await fireEvent.click(screen.getByRole('button', { name: 'Restore setup' }));
		const dialog = screen.getByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/Type Live to confirm/), {
			target: { value: 'Live' }
		});
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Restore setup' }));
		await screen.findByText('The server changed since this preview.');
		expect(screen.queryByRole('heading', { name: 'Restore preview' })).toBeNull();
		expect(daemon.requests('POST', '/instances/inst-a/setups/setup-1/restore')).toHaveLength(1);
	});
});
