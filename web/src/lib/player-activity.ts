import type { PlayerHistoryRange, PlayerObservation } from '$lib/api/players';

export interface PlayerInterval {
	from: number;
	to: number;
	players: number | null;
}

export interface PlayerActivity {
	intervals: PlayerInterval[];
	knownMs: number;
	activeMs: number;
	playerMs: number;
	peak: number | null;
	peakAt: number | null;
	average: number | null;
	coverage: number;
}

/** Counts hold until the next observation. A missing seed leaves the beginning unknown. */
export function playerActivity(
	range: PlayerHistoryRange,
	points: PlayerObservation[]
): PlayerActivity {
	const from = Date.parse(range.from);
	const to = Date.parse(range.to);
	const ordered = [...points].sort((a, b) => {
		const byTime = Date.parse(a.observed_at) - Date.parse(b.observed_at);
		return byTime || a.id.localeCompare(b.id);
	});
	const intervals: PlayerInterval[] = [];
	let start = from;
	let players = range.initial?.players ?? null;
	for (const point of ordered) {
		const at = Date.parse(point.observed_at);
		if (!Number.isFinite(at) || at < from || at >= to) continue;
		if (at > start) intervals.push({ from: start, to: at, players });
		start = at;
		players = point.players;
	}
	if (to > start) intervals.push({ from: start, to, players });

	let knownMs = 0;
	let activeMs = 0;
	let playerMs = 0;
	let peak: number | null = null;
	let peakAt: number | null = null;
	for (const interval of intervals) {
		if (interval.players === null) continue;
		const duration = interval.to - interval.from;
		knownMs += duration;
		playerMs += interval.players * duration;
		if (interval.players > 0) activeMs += duration;
		if (peak === null || interval.players > peak) {
			peak = interval.players;
			peakAt = interval.from;
		}
	}
	return {
		intervals,
		knownMs,
		activeMs,
		playerMs,
		peak,
		peakAt,
		average: knownMs > 0 ? playerMs / knownMs : null,
		coverage: to > from ? knownMs / (to - from) : 0
	};
}
