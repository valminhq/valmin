import type { Backup } from '$lib/api/backups';

/** The archives an operator would recover from. Either is null when the list holds none. */
export interface RecoveryPoints {
	newest: Backup | null;
	newestConsistent: Backup | null;
}

/**
 * Picks the newest archive of any kind and the newest consistent one by creation time, whatever
 * order the list arrives in. On equal times the earlier entry wins.
 */
export function recoveryPoints(list: Backup[]): RecoveryPoints {
	let newest: Backup | null = null;
	let newestConsistent: Backup | null = null;
	for (const archive of list) {
		const at = Date.parse(archive.created_at);
		if (!newest || at > Date.parse(newest.created_at)) newest = archive;
		if (archive.consistent && (!newestConsistent || at > Date.parse(newestConsistent.created_at))) {
			newestConsistent = archive;
		}
	}
	return { newest, newestConsistent };
}

const units: Array<[unit: Intl.RelativeTimeFormatUnit, seconds: number]> = [
	['day', 86400],
	['hour', 3600],
	['minute', 60]
];
const relative = new Intl.RelativeTimeFormat('en', { numeric: 'always' });

/** How long before `now` (epoch milliseconds) an archive was taken, in whole units: "3 hours ago". */
export function backupAge(createdAt: string, now: number): string {
	const seconds = Math.max(0, Math.floor((now - Date.parse(createdAt)) / 1000));
	for (const [unit, size] of units) {
		if (seconds >= size) return relative.format(-Math.floor(seconds / size), unit);
	}
	return 'just now';
}
