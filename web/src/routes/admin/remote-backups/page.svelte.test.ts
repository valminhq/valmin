import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RemoteDestination, RemoteSummary } from '$lib/api/remote-backups';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import Page from './+page.svelte';

let daemon: FakeDaemon;

const summary: RemoteSummary = {
	destination_id: 'dest-1',
	enabled: true,
	last_success_at: '2026-09-29T12:00:00Z',
	last_archive_at: '2026-09-29T11:00:00Z',
	last_consistent_archive_at: '2026-09-28T11:00:00Z',
	pending: 2,
	failed: 1,
	cleanup_pending: 1
};

function destination(overrides: Partial<RemoteDestination> = {}): RemoteDestination {
	return {
		id: 'dest-1',
		kind: 'webdav',
		enabled: true,
		endpoint: 'https://cloud.example.test/dav/backups',
		username: 'valmin',
		remote_name: '',
		folder: 'worlds',
		has_credentials: true,
		last_test_at: null,
		last_test_error: '',
		summary,
		...overrides
	};
}

async function open(global: string[], row: RemoteDestination | null = destination()) {
	daemon.on('GET', '/admin/remote-backup-destination', () => Response.json(row));
	session.permissions = permissions('', [], global);
	render(Page);
	if (global.includes(actions.panelSettings))
		await screen.findByRole('button', { name: 'Save destination' });
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the remote backup destination screen', () => {
	it('is closed without panel.settings', async () => {
		await open([actions.view]);
		expect(screen.getByText('You do not have permission to manage remote storage.')).toBeTruthy();
		expect(screen.queryByLabelText('WebDAV URL')).toBeNull();
		expect(daemon.requests('GET', '/admin/remote-backup-destination')).toHaveLength(0);
	});

	it('shows connection health and keeps the saved app password redacted when saving', async () => {
		await open([actions.panelSettings], destination({ last_test_error: 'Authentication failed' }));

		expect(screen.getByRole('status').textContent).toContain('Authentication failed');
		expect((screen.getByLabelText('App password') as HTMLInputElement).value).toBe('');
		expect(screen.getByPlaceholderText('Leave blank to keep saved password')).toBeTruthy();
		expect(document.body.textContent).toContain('2 pending · 1 failed · 1 awaiting remote cleanup');

		daemon.on('PUT', '/admin/remote-backup-destination', () => Response.json(destination()));
		await fireEvent.input(screen.getByLabelText('Destination folder'), {
			target: { value: 'new-worlds' }
		});
		await click(screen.getByRole('button', { name: 'Save destination' }));

		await vi.waitFor(() =>
			expect(daemon.requests('PUT', '/admin/remote-backup-destination')).toHaveLength(1)
		);
		expect(daemon.requests('PUT', '/admin/remote-backup-destination')[0].body).toEqual({
			kind: 'webdav',
			enabled: true,
			folder: 'new-worlds',
			endpoint: 'https://cloud.example.test/dav/backups',
			username: 'valmin',
			remote_name: ''
		});
	});

	it('loads configured rclone remotes before allowing an rclone destination to be saved', async () => {
		await open([actions.panelSettings], null);
		await fireEvent.change(screen.getByLabelText('Storage connection'), {
			target: { value: 'rclone' }
		});
		daemon.on('GET', '/admin/remote-backup-destination/remotes', () =>
			Response.json({ items: ['archive-drive', 's3-cold'] })
		);
		await click(screen.getByRole('button', { name: 'Load configured remotes' }));

		const remote = screen.getByLabelText('Configured remote') as HTMLInputElement;
		await vi.waitFor(() =>
			expect(
				document.querySelector('datalist#remote-names option[value="archive-drive"]')
			).toBeTruthy()
		);
		await fireEvent.input(remote, { target: { value: 'archive-drive' } });
		await fireEvent.input(screen.getByLabelText('Destination folder'), {
			target: { value: 'bucket/backups' }
		});
		daemon.on('PUT', '/admin/remote-backup-destination', () =>
			Response.json(
				destination({ kind: 'rclone', remote_name: 'archive-drive', folder: 'bucket/backups' })
			)
		);
		await click(screen.getByRole('button', { name: 'Save destination' }));

		await vi.waitFor(() =>
			expect(daemon.requests('PUT', '/admin/remote-backup-destination')).toHaveLength(1)
		);
		expect(daemon.requests('PUT', '/admin/remote-backup-destination')[0].body).toEqual({
			kind: 'rclone',
			enabled: false,
			folder: 'bucket/backups',
			endpoint: '',
			username: '',
			remote_name: 'archive-drive'
		});
	});
});
