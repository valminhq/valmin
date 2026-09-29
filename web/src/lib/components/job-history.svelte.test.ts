import { render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import type { Job } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import JobHistory from './job-history.svelte';

afterEach(() => {
	session.permissions = null;
});

const hoursAgo = (n: number) => new Date(Date.now() - n * 3600_000).toISOString();

function failedUpdate(overrides: Partial<Job> = {}): Job {
	return job({
		job_id: 'job-update',
		kind: 'game_update',
		status: 'failed',
		instance_id: 'inst-a',
		created_at: hoursAgo(26),
		started_at: '2026-09-20T12:00:00Z',
		finished_at: '2026-09-20T12:02:05Z',
		error: 'download stalled',
		error_code: 'steam_unreachable',
		requested_by_name: 'ada',
		...overrides
	});
}

function open(jobs: Job[], held: string[] = [], global: string[] = [], props = {}) {
	session.permissions = permissions('inst-a', held, global);
	return render(JobHistory, { instanceId: 'inst-a', jobs, ...props });
}

/** The row for a job: the list item holding its summary. */
const row = (name: string) => screen.getByText(name).closest('li') as HTMLElement;

describe('the operation history', () => {
	it('says plainly that nothing has run', () => {
		open([]);
		expect(screen.getByText('Nothing has run yet.')).toBeTruthy();
	});

	it('names each operation, badges its outcome and says when it happened', () => {
		const at = hoursAgo(26);
		open([
			failedUpdate({ created_at: at }),
			job({ job_id: 'job-2', kind: 'backup', status: 'succeeded' })
		]);

		const failed = row('Game update');
		expect(text(failed)).toContain('Failed');
		expect(text(failed)).toContain('yesterday');
		expect(failed.querySelector('summary [title]')?.getAttribute('title')).toBe(
			new Date(at).toLocaleString()
		);
		expect(text(row('Backup'))).toContain('Completed');
	});

	it('marks a failed row apart from the rest', () => {
		open([failedUpdate(), job({ job_id: 'job-2', kind: 'backup', status: 'succeeded' })]);
		expect(row('Game update').className).toContain('border-destructive');
		expect(row('Backup').className).not.toContain('border-destructive');
	});

	// The question this card exists to answer: which update failed, and why.
	it('opens a failed operation to who started it, how long it ran and why it failed', async () => {
		open([failedUpdate()]);
		const details = row('Game update').querySelector('details') as HTMLDetailsElement;
		expect(details.open).toBe(false);

		await click(details.querySelector('summary') as HTMLElement);
		expect(details.open).toBe(true);
		const body = text(details);
		expect(body).toContain('download stalled');
		expect(body).toContain('Code steam_unreachable');
		expect(body).toContain('Started by ada');
		expect(body).toContain('Duration 2m 5s');
		expect(body).toContain('Outcome Failed');
	});

	it('says a failure recorded no details rather than showing nothing', () => {
		open([failedUpdate({ error: undefined, error_code: undefined })]);
		expect(text(row('Game update'))).toContain('No error details recorded.');
	});

	it('credits the schedule, or the panel, when no person started an operation', () => {
		open([
			job({ job_id: 'job-1', kind: 'backup', status: 'succeeded', scheduled: true }),
			job({ job_id: 'job-2', kind: 'prune', status: 'succeeded' })
		]);
		expect(text(row('Backup'))).toContain('Started by Schedule');
		expect(text(row('Prune'))).toContain('Started by Panel');
	});

	it('lists the mods an update moved, from and to, and calls the operation an update', () => {
		open([
			job({
				kind: 'mod_install',
				status: 'succeeded',
				changes: [
					{ action: 'update', full_name: 'Foo-Bar', from_version: '1.2.0', to_version: '1.3.0' },
					{ action: 'update', full_name: 'Baz-Qux', from_version: '2.0.0', to_version: '2.1.0' }
				]
			})
		]);
		const body = text(row('Mod update'));
		expect(body).toContain('Changes');
		expect(body).toContain('Updated Foo-Bar 1.2.0 to 1.3.0');
		expect(body).toContain('Updated Baz-Qux 2.0.0 to 2.1.0');
	});

	it('words the changes of a failed operation as asked for, not as done', () => {
		open([
			job({
				kind: 'mod_toggle',
				status: 'failed',
				changes: [{ action: 'disable', full_name: 'Foo-Bar' }]
			})
		]);
		const body = text(row('Mod toggle'));
		expect(body).toContain('Changes requested');
		expect(body).toContain('Disable Foo-Bar');
		expect(body).not.toContain('Disabled Foo-Bar');
	});

	it('shows no changes for an operation that carries none', () => {
		open([job({ kind: 'mod_install', status: 'succeeded' })]);
		expect(text(row('Mod install'))).not.toContain('Changes');
	});
});

describe('where a failure leads', () => {
	const cases: Array<[kind: string, held: string, link: string, href: string]> = [
		['start', actions.consoleRead, 'Check the console', '/instances/inst-a#console'],
		['mod_install', actions.modsList, 'Open mods', '/instances/inst-a/mods'],
		['mod_uninstall', actions.modsList, 'Open mods', '/instances/inst-a/mods'],
		['mod_toggle', actions.modsList, 'Open mods', '/instances/inst-a/mods'],
		['backup', actions.backupsList, 'Open backups', '/instances/inst-a/backups'],
		['restore', actions.backupsList, 'Open backups', '/instances/inst-a/backups'],
		['game_update', actions.gameUpdate, 'See the update notice', '/instances/inst-a#update']
	];
	it.each(cases)('links a failed %s to its screen', (kind, held, link, href) => {
		open([job({ kind, status: 'failed', instance_id: 'inst-a' })], [held]);
		expect(screen.getByRole('link', { name: link }).getAttribute('href')).toBe(href);
	});

	it('offers nothing to someone who could not use the screen it leads to', () => {
		open([job({ kind: 'backup', status: 'failed', instance_id: 'inst-a' })], [actions.view]);
		expect(screen.queryByRole('link', { name: 'Open backups' })).toBeNull();
	});

	it('offers nothing after an operation that worked', () => {
		open(
			[job({ kind: 'backup', status: 'succeeded', instance_id: 'inst-a' })],
			[actions.backupsList]
		);
		expect(screen.queryByRole('link', { name: 'Open backups' })).toBeNull();
	});
});

describe('older operations', () => {
	it('asks for them only while there are more', async () => {
		const onmore = vi.fn();
		const { rerender } = open([failedUpdate()], [], [], { onmore });
		expect(screen.queryByRole('button', { name: 'Show older operations' })).toBeNull();

		await rerender({ instanceId: 'inst-a', jobs: [failedUpdate()], more: true, onmore });
		await click(screen.getByRole('button', { name: 'Show older operations' }));
		expect(onmore).toHaveBeenCalledOnce();
	});

	it('holds the control while a page is loading', () => {
		open([failedUpdate()], [], [], { more: true, loadingMore: true });
		expect((screen.getByRole('button', { name: 'Loading…' }) as HTMLButtonElement).disabled).toBe(
			true
		);
	});

	it('links to this server’s entries in the audit log only for audit.read', () => {
		open([failedUpdate()], [], [actions.auditRead]);
		expect(screen.getByRole('link', { name: 'View in the audit log' }).getAttribute('href')).toBe(
			'/admin/audit?instance_id=inst-a'
		);
	});

	it('offers no audit link without audit.read', () => {
		open([failedUpdate()]);
		expect(screen.queryByRole('link', { name: 'View in the audit log' })).toBeNull();
	});
});
