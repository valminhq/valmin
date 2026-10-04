import { render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import type { Job } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance, job, permissions } from '$lib/testing/daemon';
import { text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/maintenance') }
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));
// Pinned so no assertion depends on the machine's zone; Asia/Kolkata keeps no daylight saving.
vi.mock('$lib/api/schedules', async (importOriginal) => ({
	...(await importOriginal<typeof import('$lib/api/schedules')>()),
	viewerZone: () => 'Asia/Kolkata'
}));

const jobsPath = '/instances/inst-a/jobs';
let daemon: FakeDaemon;

async function open(runs: Job[] | Response) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(instance()));
	daemon.on('GET', '/schedules', () =>
		Response.json({ items: [], next_cursor: null, timezone: 'UTC' })
	);
	daemon.on('GET', jobsPath, () =>
		runs instanceof Response ? runs : Response.json({ items: runs, next_cursor: null })
	);
	session.permissions = permissions('inst-a', [actions.view, actions.backupsCreate]);
	render(Page);
	await vi.waitFor(() => expect(daemon.requests('GET', jobsPath)).toHaveLength(1));
}

const runsCard = () =>
	screen.getByText('Skipped and failed runs').closest('[data-slot="card"]') as HTMLElement;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	socket.reset();
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the maintenance screen', () => {
	it('shows the schedules editor beside the runs that did not complete', async () => {
		await open([]);

		expect(screen.getByRole('heading', { name: 'Maintenance' })).toBeTruthy();
		expect(await screen.findByText('Nothing is scheduled for this server.')).toBeTruthy();
		expect(screen.getByText('Skipped and failed runs')).toBeTruthy();
		const query = daemon.requests('GET', jobsPath)[0].query;
		expect(query.get('scheduled')).toBe('true');
		expect(query.get('limit')).toBe('50');
	});

	it('tells failed, skipped and cancelled runs apart and shows the error as sent', async () => {
		await open([
			job({
				job_id: 'j-1',
				kind: 'backup',
				status: 'failed',
				error: 'The world could not be archived.',
				finished_at: '2026-09-25T04:00:00Z'
			}),
			job({
				job_id: 'j-2',
				kind: 'game_update',
				status: 'cancelled',
				error_code: 'job_in_progress',
				error: 'This scheduled run was skipped: the server is modded.'
			}),
			job({ job_id: 'j-3', kind: 'restart', status: 'cancelled' }),
			job({ job_id: 'j-4', kind: 'restart', status: 'succeeded' })
		]);

		await vi.waitFor(() => expect(within(runsCard()).getAllByRole('listitem')).toHaveLength(3));
		const [failed, skipped, cancelled] = within(runsCard()).getAllByRole('listitem').map(text);
		expect(failed).toMatch(/Back up this server ?Failed/);
		expect(failed).toContain('The world could not be archived.');
		expect(failed).toMatch(/\b0?9:30\b.*Asia\/Kolkata/);
		expect(skipped).toMatch(/Update the game ?Skipped/);
		expect(skipped).toContain('This scheduled run was skipped: the server is modded.');
		expect(cancelled).toMatch(/Restart this server ?Cancelled/);
	});

	it('says how many runs it looked at when none were missed', async () => {
		await open([job({ status: 'succeeded' })]);

		expect(
			await screen.findByText('No skipped or failed runs among the last 50 scheduled runs.')
		).toBeTruthy();
	});

	it('reports a failed read', async () => {
		await open(envelope(500, 'internal', 'Valmin could not read the job history.'));

		expect(
			await within(runsCard()).findByText('Valmin could not read the job history.')
		).toBeTruthy();
		expect(screen.queryByText(/No skipped or failed runs/)).toBeNull();
	});
});
