import { describe, expect, it } from 'vitest';
import { kindLabel, nextUtc } from './schedules';

describe('nextUtc', () => {
	const from = new Date('2026-09-30T10:00:00Z'); // a Wednesday

	it.each([
		{ name: 'later today', hh: 12, mm: 30, weekday: undefined, want: '2026-09-30T12:30:00.000Z' },
		{ name: 'passed today', hh: 9, mm: 0, weekday: undefined, want: '2026-10-01T09:00:00.000Z' },
		{ name: 'exactly now', hh: 10, mm: 0, weekday: undefined, want: '2026-10-01T10:00:00.000Z' },
		{ name: 'later this week', hh: 4, mm: 0, weekday: 5, want: '2026-10-02T04:00:00.000Z' },
		{ name: 'earlier weekday', hh: 4, mm: 0, weekday: 1, want: '2026-10-05T04:00:00.000Z' },
		{ name: 'today, still ahead', hh: 23, mm: 0, weekday: 3, want: '2026-09-30T23:00:00.000Z' },
		{ name: 'today, passed', hh: 8, mm: 0, weekday: 3, want: '2026-10-07T08:00:00.000Z' }
	])('finds the next run $name', ({ hh, mm, weekday, want }) => {
		expect(nextUtc(hh, mm, from, weekday).toISOString()).toBe(want);
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
