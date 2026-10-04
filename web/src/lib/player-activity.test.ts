import { describe, expect, it } from 'vitest';
import { playerActivity } from './player-activity';
import type { PlayerHistoryRange, PlayerObservation } from './api/players';

const base = Date.parse('2026-09-01T00:00:00Z');
const at = (hours: number) => new Date(base + hours * 3_600_000).toISOString();
const point = (hours: number, players: number | null, id = String(hours)): PlayerObservation => ({
	id,
	observed_at: at(hours),
	players
});
const range = (initial: PlayerObservation | null): PlayerHistoryRange => ({
	from: at(0),
	to: at(10),
	initial
});

describe('player activity', () => {
	it('weights counts by elapsed time and excludes unknown periods', () => {
		const result = playerActivity(range(point(-1, 2)), [point(6, null), point(2, 4), point(8, 0)]);
		expect(
			result.intervals.map((interval) => [interval.from, interval.to, interval.players])
		).toEqual([
			[base, base + 2 * 3_600_000, 2],
			[base + 2 * 3_600_000, base + 6 * 3_600_000, 4],
			[base + 6 * 3_600_000, base + 8 * 3_600_000, null],
			[base + 8 * 3_600_000, base + 10 * 3_600_000, 0]
		]);
		expect(result.knownMs).toBe(8 * 3_600_000);
		expect(result.activeMs).toBe(6 * 3_600_000);
		expect(result.average).toBe(2.5);
		expect(result.playerMs / 3_600_000).toBe(20);
		expect(result.peak).toBe(4);
		expect(result.peakAt).toBe(base + 2 * 3_600_000);
		expect(result.coverage).toBe(0.8);
	});

	it('leaves the start unknown when the prior observation was pruned', () => {
		const result = playerActivity(range(null), [point(4, 0)]);
		expect(result.intervals.map((interval) => interval.players)).toEqual([null, 0]);
		expect(result.coverage).toBe(0.6);
		expect(result.peak).toBe(0);
		expect(result.average).toBe(0);
	});

	it('does not present unobserved time as an empty server', () => {
		const result = playerActivity(range(null), []);
		expect(result.knownMs).toBe(0);
		expect(result.average).toBeNull();
		expect(result.peak).toBeNull();
	});

	it('resolves simultaneous changes by ID order and preserves the first peak', () => {
		const result = playerActivity(range(point(-1, 0)), [
			point(3, 1),
			point(1, 5, 'a'),
			point(1, 3, 'b'),
			point(5, 5)
		]);
		expect(result.intervals.map((interval) => interval.players)).toEqual([0, 3, 1, 5]);
		expect(result.peakAt).toBe(base + 5 * 3_600_000);
	});
});
