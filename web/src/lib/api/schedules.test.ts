import { describe, expect, it } from 'vitest';
import { clockChanges, formatInstant, inClockChange, kindLabel } from './schedules';

describe('formatInstant', () => {
	const at = '2026-12-31T21:05:00Z';
	it.each([
		{ hour_cycle: 'h23', date_order: 'dmy', want: '31/12/2026, 21:05:00' },
		{ hour_cycle: 'h23', date_order: 'ymd', want: '2026-12-31, 21:05:00' },
		{ hour_cycle: 'h12', date_order: 'mdy', want: '12/31/2026, 9:05:00 PM' }
	] as const)('reads $hour_cycle $date_order', ({ hour_cycle, date_order, want }) => {
		expect(formatInstant(at, { timeZone: 'UTC' }, { hour_cycle, date_order })).toBe(want);
	});

	it('reads the zone given in the options', () => {
		const format = { hour_cycle: 'h23', date_order: 'ymd' } as const;
		expect(formatInstant(at, { timeZone: 'Asia/Tokyo', timeStyle: 'short' }, format)).toBe('06:05');
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
