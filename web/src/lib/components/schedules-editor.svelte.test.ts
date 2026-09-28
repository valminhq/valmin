import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import type { Schedule } from '$lib/api/schedules';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance, permissions } from '$lib/testing/daemon';
import { choose, click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import SchedulesEditor from './schedules-editor.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

// The viewer's zone is pinned so no assertion depends on the machine running the suite.
// Asia/Kolkata keeps no daylight saving, so its offset holds on any date.
const zones = vi.hoisted(() => ({ viewer: 'Asia/Kolkata' }));
vi.mock('$lib/api/schedules', async (importOriginal) => ({
	...(await importOriginal<typeof import('$lib/api/schedules')>()),
	viewerZone: () => zones.viewer
}));

let daemon: FakeDaemon;

function schedule(overrides: Partial<Schedule> = {}): Schedule {
	return {
		id: 'sch-1',
		instance_id: 'inst-a',
		kind: 'backup',
		cron: '30 3 * * *',
		enabled: true,
		last_run_at: null,
		next_run_at: '2026-09-25T03:30:00Z',
		created_by: 'u-1',
		created_by_username: 'kari',
		timezone: 'Europe/Oslo',
		wait_for_empty: false,
		max_deferral_seconds: 7200,
		unknown_players: 'wait',
		deferred_since: null,
		deferred_until: null,
		upcoming_runs: [],
		...overrides
	};
}

async function open(
	held: string[],
	{ rows = [] as Schedule[], timezone = 'Europe/Oslo' as string | null } = {}
) {
	daemon.on('GET', '/schedules', () => Response.json({ items: rows, next_cursor: null, timezone }));
	daemon.on('POST', '/schedules', () => Response.json(schedule(), { status: 201 }));
	session.permissions = permissions('inst-a', held);
	render(SchedulesEditor, { instance: instance() });
	if (held.length > 0)
		await vi.waitFor(() => expect(daemon.requests('GET', '/schedules')).toHaveLength(1));
}

const created = () => daemon.requests('POST', '/schedules');
const add = () => screen.getByRole('button', { name: 'Add schedule' }) as HTMLButtonElement;

const upcoming = () =>
	within(screen.getByRole('heading', { name: 'Upcoming runs' }).closest('section') as HTMLElement)
		.queryAllByRole('listitem')
		.map(text);

beforeEach(() => {
	zones.viewer = 'Asia/Kolkata';
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	socket.reset();
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the schedules editor', () => {
	// ADR-132: a schedule is authorized by the action its run would exercise, so the kinds on
	// offer are the ones this member could run by hand.
	it('offers only the kinds whose action the member holds', async () => {
		await open([actions.backupsCreate]);

		await fireEvent.pointerDown(screen.getByLabelText('What to run'), {
			button: 0,
			pointerType: 'mouse'
		});
		const options = (await screen.findAllByRole('option')).map((o) => o.textContent?.trim());
		expect(options).toEqual(['Back up this server']);
	});

	it('is closed to a member who can run none of them', async () => {
		await open([actions.view]);
		expect(screen.getByText('Scheduling is not available to you.')).toBeTruthy();
	});

	// The builder writes an expression from its controls, and says what it means from the
	// controls rather than by reading the string back.
	it('builds a daily expression from the time chosen', async () => {
		await open([actions.backupsCreate]);
		await choose(screen.getByLabelText('What to run'), 'Back up this server');
		await fireEvent.input(screen.getByLabelText('Time of day'), { target: { value: '23:15' } });

		expect(text(document.body)).toContain('Every day at 23:15 · 15 23 * * *');
		await click(add());
		await vi.waitFor(() => expect(created()).toHaveLength(1));
		expect(created()[0].body).toEqual({
			instance_id: 'inst-a',
			kind: 'backup',
			cron: '15 23 * * *',
			wait_for_empty: false,
			max_deferral_seconds: 7200,
			unknown_players: 'wait'
		});
	});

	it('builds a weekly expression on the day chosen', async () => {
		await open([actions.backupsCreate]);
		await choose(screen.getByLabelText('What to run'), 'Back up this server');
		await choose(screen.getByLabelText('How often'), 'Every week');
		await choose(screen.getByLabelText('Day of the week'), 'Monday');

		await click(add());
		await vi.waitFor(() => expect(created()).toHaveLength(1));
		expect(created()[0].body).toMatchObject({ cron: '00 04 * * 1' });
	});

	// A step that does not divide 24 wraps unevenly: every 5 hours fires at 20:00 and again
	// four hours later at midnight.
	it('offers only hour steps that divide the day, and names the times they fire', async () => {
		await open([actions.backupsCreate]);
		await choose(screen.getByLabelText('How often'), 'Every few hours');

		await fireEvent.pointerDown(screen.getByLabelText('Hours between runs'), {
			button: 0,
			pointerType: 'mouse'
		});
		for (const option of await screen.findAllByRole('option')) {
			const n = Number(option.textContent?.trim().split(' ')[0]);
			expect(24 % n, `${n} hours`).toBe(0);
		}
		await fireEvent.pointerUp(screen.getByRole('option', { name: '6 hours' }), {
			button: 0,
			pointerType: 'mouse'
		});
		expect(text(document.body)).toContain(
			'Every 6 hours — 00:00, 06:00, 12:00, 18:00 · 0 */6 * * *'
		);
	});

	// The expression is the daemon's to validate; the SPA sends it as written and shows the
	// daemon's answer.
	it('sends a written expression as it is, and shows the daemon’s refusal', async () => {
		await open([actions.backupsCreate]);
		daemon.on('POST', '/schedules', () =>
			envelope(422, 'validation_failed', 'That schedule cannot be read.', [
				{ field: 'cron', code: 'invalid', message: 'Five fields are needed.' }
			])
		);
		await choose(screen.getByLabelText('What to run'), 'Back up this server');
		await choose(screen.getByLabelText('How often'), 'Cron expression');
		await fireEvent.input(screen.getByLabelText('Cron expression'), {
			target: { value: ' 7 7 7 ' }
		});

		await click(add());
		await vi.waitFor(() => expect(created()).toHaveLength(1));
		expect(created()[0].body).toMatchObject({ cron: '7 7 7' });
		expect(await screen.findByText(/That schedule cannot be read\./)).toBeTruthy();
	});

	// An operator reading "04:00" and assuming their own clock is the misunderstanding this
	// names away.
	it('says which timezone every time is in', async () => {
		await open([actions.backupsCreate], { rows: [schedule()] });

		expect(await screen.findByText(/times in Europe\/Oslo/)).toBeTruthy();
		expect(
			screen.getByText(/Times are the server’s, in Europe\/Oslo — not your own clock\./)
		).toBeTruthy();
		expect(screen.getByText('30 3 * * *'), 'the expression it was created with').toBeTruthy();
	});

	// Next and last run are shown on the clock the row's label names, not the browser's. No
	// browser zone sits at both offsets, so at least one row fails if the browser's is used.
	it.each([
		{ timezone: 'UTC', next: /next [^·]*\b0?3:30\b/, last: /last [^·]*\b0?1:15\b/ },
		{ timezone: 'Asia/Kolkata', next: /next [^·]*\b0?9:00\b/, last: /last [^·]*\b0?6:45\b/ }
	])('shows next and last run in the row’s timezone ($timezone)', async (c) => {
		const row = schedule({ timezone: c.timezone, last_run_at: '2026-09-24T01:15:00Z' });
		await open([actions.backupsCreate], { rows: [row], timezone: c.timezone });

		const line = text(await screen.findByText(new RegExp(`times in ${c.timezone}`)));
		expect(line).toMatch(c.next);
		expect(line).toMatch(c.last);
	});

	it('refuses to create a schedule while the server’s timezone is unknown', async () => {
		await open([actions.backupsCreate], { timezone: null });
		await choose(screen.getByLabelText('What to run'), 'Back up this server');

		expect(await screen.findByText(/Scheduler timezone is unavailable/)).toBeTruthy();
		expect(add().disabled).toBe(true);
	});

	it('pauses a schedule through the daemon', async () => {
		await open([actions.backupsCreate], { rows: [schedule()] });
		daemon.on('PATCH', '/schedules/sch-1', () => Response.json(schedule({ enabled: false })));

		await click(await screen.findByRole('switch', { name: 'Enable Back up this server schedule' }));
		await vi.waitFor(() => expect(daemon.requests('PATCH', '/schedules/sch-1')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/schedules/sch-1')[0].body).toEqual({ enabled: false });
	});

	// F5: deleting stops something that runs unattended, so it is named and typed back.
	it('deletes a schedule only after its expression is typed back', async () => {
		await open([actions.backupsCreate], { rows: [schedule()] });
		daemon.on('DELETE', '/schedules/sch-1', () => new Response(null, { status: 204 }));

		await click(await screen.findByRole('button', { name: 'Delete schedule' }));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('Back up this server stops running on its own.');
		const confirm = within(dialog).getByRole('button', { name: 'Delete' });
		await click(confirm);
		expect(daemon.requests('DELETE', '/schedules/sch-1'), 'not before the name').toHaveLength(0);

		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: '30 3 * * *' }
		});
		await click(confirm);
		await vi.waitFor(() => expect(daemon.requests('DELETE', '/schedules/sch-1')).toHaveLength(1));
	});

	it('offers the player policy only for kinds that stop the server', async () => {
		await open([actions.backupsCreate, actions.restart, actions.gameUpdate]);

		await choose(screen.getByLabelText('What to run'), 'Update the game');
		expect(screen.queryByLabelText('Wait until no players are connected')).toBeNull();
		await choose(screen.getByLabelText('What to run'), 'Restart this server');
		expect(screen.getByLabelText('Wait until no players are connected')).toBeTruthy();
		await choose(screen.getByLabelText('What to run'), 'Back up this server');
		expect(screen.getByLabelText('Wait until no players are connected')).toBeTruthy();
		expect(screen.queryByLabelText('Wait at most'), 'hidden until waiting is on').toBeNull();
	});

	it('sends the player policy with a restart schedule', async () => {
		await open([actions.restart]);
		await choose(screen.getByLabelText('What to run'), 'Restart this server');
		await click(screen.getByLabelText('Wait until no players are connected'));
		await choose(screen.getByLabelText('Wait at most'), '4 hours');
		await choose(
			screen.getByLabelText('If the player count is unknown'),
			'Run, as if the server is empty'
		);

		await click(add());
		await vi.waitFor(() => expect(created()).toHaveLength(1));
		expect(created()[0].body).toEqual({
			instance_id: 'inst-a',
			kind: 'restart',
			cron: '00 04 * * *',
			wait_for_empty: true,
			max_deferral_seconds: 14400,
			unknown_players: 'run'
		});
	});

	it('shows a held run and the latest time it starts', async () => {
		const row = schedule({
			kind: 'restart',
			timezone: 'UTC',
			wait_for_empty: true,
			deferred_since: '2026-09-25T03:30:00Z',
			deferred_until: '2026-09-25T05:30:00Z'
		});
		await open([actions.restart], { rows: [row], timezone: 'UTC' });

		expect(await screen.findByText('waiting for players')).toBeTruthy();
		expect(text(document.body)).toContain('waits up to 2 hours for players to leave');
		const line = text(screen.getByText(/Waiting since/));
		expect(line).toMatch(/Waiting since [^.]*\b0?3:30\b/);
		expect(line).toMatch(/or at [^.]*\b0?5:30\b[^.]* at the latest/);
	});

	it('reads the schedules again when a hold starts or ends', async () => {
		await open([actions.restart]);
		socket.push('instance.inst-a.state', { type: 'maintenance', instance: 'inst-a' });
		await vi.waitFor(() => expect(daemon.requests('GET', '/schedules')).toHaveLength(2));
	});

	it('lists the next runs of enabled schedules earliest first, on both clocks', async () => {
		const rows = [
			schedule({
				id: 'a',
				timezone: 'UTC',
				upcoming_runs: ['2026-09-26T04:00:00Z', '2026-09-27T04:00:00Z']
			}),
			schedule({
				id: 'b',
				kind: 'restart',
				timezone: 'UTC',
				upcoming_runs: ['2026-09-25T03:00:00Z']
			}),
			schedule({ id: 'c', kind: 'game_update', timezone: 'UTC', enabled: false })
		];
		await open([actions.backupsCreate], { rows, timezone: 'UTC' });

		await screen.findByRole('heading', { name: 'Upcoming runs' });
		const items = upcoming();
		expect(items).toHaveLength(3);
		expect(items[0]).toMatch(
			/Restart this server.*\b0?3:00\b[^·]*UTC.*\b0?8:30\b.*your time \(Asia\/Kolkata\)/
		);
		expect(items[1]).toMatch(/Back up this server.*\b0?4:00\b[^·]*UTC.*\b0?9:30\b.*your time/);
		expect(items.join(' ')).not.toContain('Update the game');
	});

	it('lists a held run first and caps the upcoming runs at eight', async () => {
		const runs = [1, 2, 3, 4, 5].map((d) => `2026-10-0${d}T04:00:00Z`);
		const rows = [
			schedule({ id: 'a', timezone: 'UTC', upcoming_runs: runs }),
			schedule({
				id: 'b',
				kind: 'restart',
				timezone: 'UTC',
				wait_for_empty: true,
				deferred_since: '2026-09-25T03:30:00Z',
				deferred_until: '2026-09-25T05:30:00Z',
				upcoming_runs: runs.map((r) => r.replace('04:00', '03:00'))
			})
		];
		await open([actions.backupsCreate, actions.restart], { rows, timezone: 'UTC' });

		await screen.findByRole('heading', { name: 'Upcoming runs' });
		const items = upcoming();
		expect(items).toHaveLength(9);
		expect(items[0]).toMatch(
			/Restart this server ?Waiting for players to leave\. Runs at [^.]*\b0?5:30\b[^.]*UTC at the latest\./
		);
	});

	it('shows a run time once when the viewer shares the schedule’s zone', async () => {
		zones.viewer = 'UTC';
		const rows = [schedule({ timezone: 'UTC', upcoming_runs: ['2026-09-26T04:00:00Z'] })];
		await open([actions.backupsCreate], { rows, timezone: 'UTC' });

		await screen.findByRole('heading', { name: 'Upcoming runs' });
		expect(upcoming()[0]).toMatch(/\b0?4:00\b[^·]*UTC/);
		expect(text(document.body)).not.toContain('your time');
	});

	it('says no runs are coming up while every schedule is paused', async () => {
		await open([actions.backupsCreate], { rows: [schedule({ enabled: false })] });

		expect(await screen.findByText('No runs are coming up.')).toBeTruthy();
		expect(upcoming()).toHaveLength(0);
	});

	it('gives a schedule’s next run on the viewer’s clock too', async () => {
		await open([actions.backupsCreate], { rows: [schedule()] });

		const line = text(await screen.findByText(/Next run in your time/));
		expect(line).toMatch(/\b0?9:00\b.*\(Asia\/Kolkata\)/);
	});

	it.each([
		{
			name: 'a daily time',
			pick: async () => {
				await fireEvent.input(screen.getByLabelText('Time of day'), {
					target: { value: '23:15' }
				});
			},
			want: '23:15 UTC is 04:45 your time (Asia/Kolkata)'
		},
		{
			name: 'a weekly time, on the local weekday',
			pick: async () => {
				await choose(screen.getByLabelText('How often'), 'Every week');
				await choose(screen.getByLabelText('Day of the week'), 'Monday');
				await fireEvent.input(screen.getByLabelText('Time of day'), {
					target: { value: '23:00' }
				});
			},
			want: 'Monday 23:00 UTC is Tuesday 04:30 your time (Asia/Kolkata)'
		},
		{
			name: 'every few hours',
			pick: async () => {
				await choose(screen.getByLabelText('How often'), 'Every few hours');
			},
			want: 'In your time (Asia/Kolkata): 05:30, 11:30, 17:30, 23:30'
		}
	])('previews $name on the viewer’s clock', async ({ pick, want }) => {
		await open([actions.backupsCreate], { timezone: 'UTC' });
		await pick();

		expect(text(document.body)).toContain(want);
	});

	it('lists every-few-hours times in local clock order west of UTC', async () => {
		zones.viewer = 'America/Phoenix';
		await open([actions.backupsCreate], { timezone: 'UTC' });
		await choose(screen.getByLabelText('How often'), 'Every few hours');

		expect(text(document.body)).toContain(
			'In your time (America/Phoenix): 05:00, 11:00, 17:00, 23:00'
		);
	});

	it.each([
		{ name: 'the viewer is in UTC', viewer: 'UTC', timezone: 'UTC' },
		{ name: 'the scheduler is not in UTC', viewer: 'Asia/Kolkata', timezone: 'Europe/Oslo' }
	])('gives no local preview when $name', async ({ viewer, timezone }) => {
		zones.viewer = viewer;
		await open([actions.backupsCreate], { timezone });
		await screen.findByText(/Every day at 04:00/);

		expect(text(document.body)).not.toContain('your time');
	});

	it('gives a written expression no preview and points to its upcoming runs', async () => {
		await open([actions.backupsCreate], { timezone: 'UTC' });
		await choose(screen.getByLabelText('How often'), 'Cron expression');

		expect(text(document.body)).not.toContain('your time');
		expect(text(document.body)).toContain('Once added, its next runs appear under Upcoming runs.');
	});
});
