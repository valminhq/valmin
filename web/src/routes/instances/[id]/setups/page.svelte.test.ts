import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import type { ManifestLaunch } from '$lib/api/manifest';
import type { SetupDetail, SetupPreview } from '$lib/api/setups';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, backup, envelope, instance, job, permissions } from '$lib/testing/daemon';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/setups') }
}));

vi.mock('$app/state', () => app);
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

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
	configs: [{ file: 'plugin.cfg', content: 'enabled = false\n' }]
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
			configs: [{ file: 'plugin.cfg', content: 'enabled = true\n' }]
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
