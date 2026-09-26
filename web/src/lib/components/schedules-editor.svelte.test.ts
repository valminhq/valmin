import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import type { Schedule } from '$lib/api/schedules';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, instance, permissions } from '$lib/testing/daemon';
import { choose, click, text } from '$lib/testing/interact';
import SchedulesEditor from './schedules-editor.svelte';

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

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
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
			cron: '15 23 * * *'
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
});
