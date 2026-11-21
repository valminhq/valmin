import { describe, expect, it } from 'vitest';
import {
	buildCron,
	clockChanges,
	defaultCronForm,
	describeCron,
	describeExpression,
	describeSteps,
	hourChoices,
	monthDays,
	readCron,
	type CronForm,
	formatInstant,
	inClockChange,
	kindLabel,
	stopsServer,
	waitWords
} from './schedules';

describe('waitWords', () => {
	it.each([
		{ seconds: 1, want: '1 second' },
		{ seconds: 45, want: '45 seconds' },
		{ seconds: 90, want: '90 seconds' },
		{ seconds: 300, want: '5 minutes' },
		{ seconds: 3600, want: '1 hour' }
	])('says $seconds seconds as $want', ({ seconds, want }) => {
		expect(waitWords(seconds)).toBe(want);
	});
});

describe('describeSteps', () => {
	it('reads a chain in one line', () => {
		expect(
			describeSteps([
				{ type: 'command', command: 'say Restarting' },
				{ type: 'wait', seconds: 300 },
				{ type: 'stop' },
				{ type: 'game_update' },
				{ type: 'start' }
			])
		).toBe('send "say Restarting" → wait 5 minutes → stop → update the game → start');
	});

	it('reads a wait for the server with its condition and limit', () => {
		expect(
			describeSteps([
				{ type: 'start' },
				{ type: 'wait_for', condition: 'ready', seconds: 120 },
				{ type: 'wait_for', condition: 'empty', seconds: 600 }
			])
		).toBe(
			'start → wait up to 2 minutes for the server to be ready → wait up to 10 minutes for the server to have no players'
		);
	});
});

describe('stopsServer', () => {
	it.each([
		{
			steps: [
				{ type: 'command', command: 'save' },
				{ type: 'wait', seconds: 5 }
			],
			want: false
		},
		{ steps: [{ type: 'start' }], want: false },
		{ steps: [{ type: 'command', command: 'save' }, { type: 'restart' }], want: true },
		{ steps: [{ type: 'backup' }], want: true }
	] as const)('is $want for $steps', ({ steps, want }) => {
		expect(stopsServer([...steps])).toBe(want);
	});
});

describe('formatInstant', () => {
	const at = '2026-12-31T21:05Z';
	it.each([
		{ hour_cycle: 'h23', date_order: 'dmy', want: '31/12/2026, 21:05' },
		{ hour_cycle: 'h23', date_order: 'ymd', want: '2026-12-31, 21:05' },
		{ hour_cycle: 'h12', date_order: 'mdy', want: '12/31/2026, 9:05 PM' }
	] as const)('reads $hour_cycle $date_order', ({ hour_cycle, date_order, want }) => {
		expect(formatInstant(at, { timeZone: 'UTC' }, { hour_cycle, date_order })).toBe(want);
	});

	it('reads the zone given in the options', () => {
		const format = { hour_cycle: 'h23', date_order: 'ymd' } as const;
		expect(formatInstant(at, { timeZone: 'Asia/Tokyo', timeStyle: 'short' }, format)).toBe('06:05');
	});

	it('shows only the fields a caller names, and adds seconds to the default', () => {
		const format = { hour_cycle: 'h23', date_order: 'dmy' } as const;
		const utc = { timeZone: 'UTC' };
		expect(formatInstant(at, { ...utc, hour: '2-digit', minute: '2-digit' }, format)).toBe('21:05');
		expect(formatInstant(at, { ...utc, month: 'short', day: 'numeric' }, format)).toBe('31 Dec');
		expect(formatInstant(at, { ...utc, second: '2-digit' }, format)).toBe('31/12/2026, 21:05:00');
	});
});

describe('kindLabel', () => {
	it.each([
		{ kind: 'backup', want: 'Back up this server' },
		{ kind: 'game_update', want: 'Update the game' },
		{ kind: 'update_check', want: 'update check' }
	])('labels $kind', ({ kind, want }) => {
		expect(kindLabel(kind)).toBe(want);
	});
});

describe('clockChanges', () => {
	const from = Date.parse('2026-10-07T12:00:00Z');
	const window = (start: string, end: string) => {
		const m = (t: string) => Number(t.slice(0, 2)) * 60 + Number(t.slice(3));
		return { start: m(start), end: m(end) };
	};

	it.each([
		{
			zone: 'Europe/Berlin',
			want: [
				{ kind: 'repeated', ...window('02:00', '03:00') },
				{ kind: 'skipped', ...window('02:00', '03:00') }
			]
		},
		{
			zone: 'America/New_York',
			want: [
				{ kind: 'repeated', ...window('01:00', '02:00') },
				{ kind: 'skipped', ...window('02:00', '03:00') }
			]
		},
		// Half an hour of daylight saving, with the southern hemisphere's order.
		{
			zone: 'Australia/Lord_Howe',
			want: [
				{ kind: 'repeated', ...window('01:30', '02:00') },
				{ kind: 'skipped', ...window('02:00', '02:30') }
			]
		},
		{ zone: 'UTC', want: [] },
		{ zone: 'Atlantic/Reykjavik', want: [] },
		{ zone: 'Asia/Kolkata', want: [] }
	])('finds the windows of $zone', ({ zone, want }) => {
		expect(clockChanges(zone, from)).toEqual(want);
	});

	it.each([
		{
			at: 'just after a change, when the next one is over a year away',
			from: '2026-10-25T12:00:00Z'
		},
		{ at: 'when the same change falls twice in the scan', from: '2026-03-23T12:00:00Z' }
	])('finds each window of Europe/Berlin once $at', ({ from }) => {
		const found = clockChanges('Europe/Berlin', Date.parse(from));
		expect(found).toHaveLength(2);
		expect(found).toEqual(
			expect.arrayContaining([
				{ kind: 'repeated', ...window('02:00', '03:00') },
				{ kind: 'skipped', ...window('02:00', '03:00') }
			])
		);
	});

	it('wraps a window that crosses midnight', () => {
		const change = { kind: 'repeated' as const, start: 1410, end: 1470 };
		expect([1409, 1410, 0, 29, 30].map((m) => inClockChange(m, change))).toEqual([
			false,
			true,
			true,
			true,
			false
		]);
	});
});

describe('readCron', () => {
	const form = (f: Partial<CronForm>): CronForm => ({ ...defaultCronForm(), ...f });

	// Every expression the builder can write reads back into controls that write it again.
	it('reads back every expression the builder writes', () => {
		const forms: CronForm[] = [];
		for (const atTime of ['00:00', '04:05', '23:59']) {
			forms.push(form({ every: 'day', atTime }));
			for (const monthDay of monthDays) forms.push(form({ every: 'month', atTime, monthDay }));
			for (const weekdays of [['0'], ['1', '3'], ['6', '0', '2'], ['1', '2', '3', '4', '5', '6']])
				forms.push(form({ every: 'days', atTime, weekdays }));
		}
		for (const everyHours of hourChoices)
			for (const startAt of ['00:00', '05:30', '11:07', '23:45'])
				forms.push(form({ every: 'hours', everyHours, startAt }));

		for (const f of forms) {
			const expr = buildCron(f);
			const read = readCron(expr);
			expect(read.every, expr).not.toBe('custom');
			expect(buildCron(form(read)), expr).toBe(expr);
		}
	});

	it.each([
		{ expr: '30 03 * * *', want: { every: 'day', atTime: '03:30' } },
		{ expr: '00 04 * * 1,3', want: { every: 'days', atTime: '04:00', weekdays: ['1', '3'] } },
		{ expr: '15 22 22 * *', want: { every: 'month', atTime: '22:15', monthDay: '22' } },
		{ expr: '30 5/6 * * *', want: { every: 'hours', everyHours: '6', startAt: '05:30' } },
		{ expr: '0 */12 * * *', want: { every: 'hours', everyHours: '12', startAt: '00:00' } }
	])('reads $expr into the controls', ({ expr, want }) => {
		expect(readCron(expr)).toEqual(want);
	});

	// A hand-written daily, weekly or monthly expression may leave off the leading zero.
	it.each([
		{ expr: '0 4 * * *', want: { every: 'day', atTime: '04:00' } },
		{ expr: '5 22 * * 1,3', want: { every: 'days', atTime: '22:05', weekdays: ['1', '3'] } },
		{ expr: '0 4 22 * *', want: { every: 'month', atTime: '04:00', monthDay: '22' } }
	])('reads the unpadded $expr into the controls', ({ expr, want }) => {
		expect(readCron(expr)).toEqual(want);
	});

	// None of these is what the builder writes, so none is interpreted.
	it.each([
		'@daily',
		'0 */5 * * *',
		'00 04 * * 7',
		'00 04 * * 1-5',
		'00 04 * * 3,1',
		'00 04 * * 1,1',
		'00 04 * * 0,1,2,3,4,5,6',
		'00 04 31 * *',
		'00 04 01 * *',
		'0 7/6 * * *',
		'0 0/6 * * *',
		'05 */6 * * *',
		'60 04 * * *',
		'0 24 * * *',
		'00 24 * * *',
		' 00 04 * * *'
	])('keeps %j as a written expression', (expr) => {
		expect(readCron(expr)).toEqual({ every: 'custom', custom: expr });
	});
});

describe('describeExpression', () => {
	it.each([
		{ expr: '0 4 * * *', want: 'Every day at 04:00' },
		{ expr: '00 04 * * 1,3', want: 'Every Monday and Wednesday at 04:00' },
		{ expr: '15 22 22 * *', want: 'Every month on the 22nd at 22:15' },
		{ expr: '0 */12 * * *', want: 'Every 12 hours — 00:00, 12:00' }
	])('says $expr in words', ({ expr, want }) => {
		expect(describeExpression(expr)).toBe(want);
	});

	// Only the daemon knows what a hand-written expression means, so the SPA says nothing.
	it.each(['@daily', '0 4 * * 1-5', '*/5 * * * *'])('says nothing for %j', (expr) => {
		expect(describeExpression(expr)).toBe('');
	});

	it('asks for a day when none is chosen', () => {
		expect(describeCron({ ...defaultCronForm(), every: 'days', weekdays: [] })).toBe(
			'Choose at least one day'
		);
	});
});
