import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RemoteCopy, RemoteCopyPage, RemoteSummary } from '$lib/api/remote-backups';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import RemoteBackupsPanel from './remote-backups-panel.svelte';

let daemon: FakeDaemon;

const summary: RemoteSummary = {
	destination_id: 'dest-1',
	enabled: true,
	last_success_at: '2026-09-29T12:00:00Z',
	last_archive_at: '2026-09-29T11:00:00Z',
	last_consistent_archive_at: '2026-09-28T11:00:00Z',
	pending: 1,
	failed: 1,
	cleanup_pending: 0
};

function copy(overrides: Partial<RemoteCopy> = {}): RemoteCopy {
	return {
		id: 'copy-1',
		destination_id: 'dest-1',
		backup_id: 'backup-1',
		instance_id: 'inst-a',
		world_name: 'Midgard',
		consistent: true,
		archive_created_at: '2026-09-29T11:00:00Z',
		status: 'pending',
		attempts: 0,
		next_attempt_at: '',
		deadline_at: '',
		last_error: '',
		succeeded_at: null,
		job_id: null,
		cleanup_pending: false,
		cleanup_error: '',
		source_available: true,
		...overrides
	};
}

function page(items: RemoteCopy[], remoteSummary = summary): RemoteCopyPage {
	return { items, next_cursor: null, summary: remoteSummary };
}

async function open(
	held: string[],
	rows: RemoteCopy[] = [copy()],
	globalActions: string[] = [],
	remoteSummary: RemoteSummary = summary
) {
	daemon.on('GET', '/instances/inst-a/remote-copies', () =>
		Response.json(page(rows, remoteSummary))
	);
	session.permissions = permissions('inst-a', held, globalActions);
	render(RemoteBackupsPanel, {
		instance: instance({
			id: 'inst-a',
			name: 'Midgard',
			remote_backup_enabled: true,
			remote_keep_cold: 10,
			remote_keep_hot: 5,
			remote_keep_snapshots: 10
		})
	});
	if (held.includes(actions.backupsList)) {
		if (remoteSummary.destination_id) await screen.findByText('1 pending · 1 failed');
		else await screen.findByText(/Remote storage is not configured/);
	}
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the remote backups panel', () => {
	it('shows remote health and persists the per-server retention policy', async () => {
		await open([actions.backupsList, actions.settings]);
		expect(document.body.textContent).toContain('Latest consistent remote backup');
		expect(document.body.textContent).toContain('Midgard');

		daemon.on('PATCH', '/instances/inst-a', () =>
			Response.json(
				instance({
					id: 'inst-a',
					remote_backup_enabled: true,
					remote_keep_cold: 0,
					remote_keep_hot: 5,
					remote_keep_snapshots: 10
				})
			)
		);
		const cold = screen.getByLabelText('Consistent copies to keep') as HTMLInputElement;
		await fireEvent.input(cold, { target: { value: '0' } });
		await vi.waitFor(() => expect(cold.value).toBe('0'));
		const save = screen.getByRole('button', { name: 'Save remote policy' }) as HTMLButtonElement;
		await vi.waitFor(() => expect(save.disabled).toBe(false));
		await click(save);

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/instances/inst-a')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/instances/inst-a')[0].body).toEqual({
			remote_backup_enabled: true,
			remote_keep_cold: 0,
			remote_keep_hot: 5,
			remote_keep_snapshots: 10
		});
	});

	it('cancels a pending copy and retries a failed copy when the destination is enabled', async () => {
		const pending = copy({ id: 'pending-1' });
		const failed = copy({ id: 'failed-1', status: 'failed', last_error: 'temporary error' });
		await open([actions.backupsList, actions.backupsCreate], [pending, failed]);
		daemon.on(
			'POST',
			'/instances/inst-a/remote-copies/pending-1/cancel',
			() => new Response(null, { status: 204 })
		);
		daemon.on('POST', '/instances/inst-a/remote-copies/failed-1/retry', () =>
			Response.json({ copy_id: 'failed-1', status: 'pending', job_id: 'job-2' })
		);

		await click(screen.getAllByRole('button', { name: 'Cancel upload' })[0]);
		await vi.waitFor(() =>
			expect(
				daemon.requests('POST', '/instances/inst-a/remote-copies/pending-1/cancel')
			).toHaveLength(1)
		);
		await vi.waitFor(() =>
			expect(
				(screen.getByRole('button', { name: 'Retry upload' }) as HTMLButtonElement).disabled
			).toBe(false)
		);
		await click(screen.getByRole('button', { name: 'Retry upload' }));
		await vi.waitFor(() =>
			expect(
				daemon.requests('POST', '/instances/inst-a/remote-copies/failed-1/retry')
			).toHaveLength(1)
		);
	});

	it('holds the retention policy until a destination is configured', async () => {
		await open([actions.backupsList, actions.settings], [], [], {
			...summary,
			destination_id: null
		});
		for (const label of [
			'Automatically upload new backups',
			'Consistent copies to keep',
			'Best-effort copies to keep',
			'Safety snapshots to keep'
		]) {
			expect((screen.getByLabelText(label) as HTMLInputElement).disabled, label).toBe(true);
		}
		expect(
			(screen.getByRole('button', { name: 'Save remote policy' }) as HTMLButtonElement).disabled
		).toBe(true);
		expect(screen.queryByText(/No remote copies yet/)).toBeNull();
	});

	it('says there are no remote copies yet once a destination is configured', async () => {
		daemon.on('GET', '/instances/inst-a/remote-copies', () => Response.json(page([])));
		session.permissions = permissions('inst-a', [actions.backupsList]);
		render(RemoteBackupsPanel, { instance: instance({ id: 'inst-a' }) });
		expect(await screen.findByText(/No remote copies yet/)).toBeTruthy();
	});

	it('shows setup guidance to a member without panel settings access', async () => {
		await open([actions.backupsList], [copy()], [], { ...summary, destination_id: null });
		expect(screen.getByText(/Ask an administrator to configure it/)).toBeTruthy();
		expect(screen.queryByRole('link', { name: 'Configure destination' })).toBeNull();
	});

	it('links to destination setup for a member with panel settings access', async () => {
		await open([actions.backupsList], [copy()], [actions.panelSettings], {
			...summary,
			destination_id: null
		});
		expect(
			screen.getByRole('link', { name: 'Configure destination' }).getAttribute('href')
		).toContain('/admin/remote-backups');
	});
});
