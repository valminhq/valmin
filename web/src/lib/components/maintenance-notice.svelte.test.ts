import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Schedule } from '$lib/api/schedules';
import { FakeDaemon } from '$lib/testing/daemon';
import { text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import MaintenanceNotice from './maintenance-notice.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;
let rows: Schedule[] = [];

function schedule(overrides: Partial<Schedule> = {}): Schedule {
	return {
		id: 'sch-1',
		instance_id: 'inst-a',
		kind: 'restart',
		cron: '0 4 * * *',
		enabled: true,
		last_run_at: null,
		next_run_at: '2026-09-25T04:00:00Z',
		created_by: null,
		created_by_username: null,
		timezone: 'UTC',
		wait_for_empty: true,
		max_deferral_seconds: 7200,
		unknown_players: 'wait',
		deferred_since: null,
		deferred_until: null,
		upcoming_runs: [],
		...overrides
	};
}

const held = (overrides: Partial<Schedule> = {}) =>
	schedule({
		deferred_since: '2026-09-25T04:00:00Z',
		deferred_until: '2026-09-25T06:00:00Z',
		...overrides
	});

beforeEach(() => {
	rows = [];
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/schedules', () =>
		Response.json({ items: rows, next_cursor: null, timezone: 'UTC' })
	);
});

afterEach(() => {
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the maintenance notice', () => {
	it('names a held run and the latest time it starts', async () => {
		rows = [held()];
		render(MaintenanceNotice, { instanceId: 'inst-a' });

		expect(
			await screen.findByText('Scheduled restart is waiting for players to leave')
		).toBeTruthy();
		expect(text(screen.getByRole('alert'))).toMatch(
			/or at [^(]*\b0?6:00\b[^(]*\(UTC\) at the latest/
		);
	});

	it('shows only the held runs of this server', async () => {
		rows = [
			schedule(),
			held({ id: 'sch-2', instance_id: 'inst-b' }),
			held({ id: 'sch-3', kind: 'backup' })
		];
		render(MaintenanceNotice, { instanceId: 'inst-a' });

		expect(
			await screen.findByText('Scheduled backup is waiting for players to leave')
		).toBeTruthy();
		expect(screen.getAllByRole('alert')).toHaveLength(1);
	});

	it('appears when the daemon signals a hold', async () => {
		render(MaintenanceNotice, { instanceId: 'inst-a' });
		await vi.waitFor(() => expect(daemon.requests('GET', '/schedules')).toHaveLength(1));

		rows = [held({ kind: 'backup' })];
		socket.push('instance.inst-a.state', { type: 'maintenance', instance: 'inst-a' });
		expect(
			await screen.findByText('Scheduled backup is waiting for players to leave')
		).toBeTruthy();
	});
});
