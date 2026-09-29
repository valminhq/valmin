import { describe, expect, it } from 'vitest';
import type { Backup } from '$lib/api/backups';
import { backupAge, recoveryPoints } from '$lib/backup-points';

function archive(id: string, createdAt: string, consistent: boolean): Backup {
	return {
		id,
		instance_id: 'one',
		size_bytes: 1,
		sha256: '',
		world_name: 'World',
		trigger: 'manual',
		consistent,
		created_at: createdAt,
		filename: `${id}.tar.gz`,
		prunes_next: false
	};
}

describe('recoveryPoints', () => {
	const cases: Array<{
		name: string;
		list: Backup[];
		newest: string | null;
		newestConsistent: string | null;
	}> = [
		{ name: 'an empty list has neither', list: [], newest: null, newestConsistent: null },
		{
			name: 'a consistent newest archive is both',
			list: [
				archive('a', '2026-09-20T12:00:00Z', true),
				archive('b', '2026-09-19T12:00:00Z', true)
			],
			newest: 'a',
			newestConsistent: 'a'
		},
		{
			name: 'a best-effort newest archive leaves the consistent one older',
			list: [
				archive('hot', '2026-09-20T12:00:00Z', false),
				archive('cold', '2026-09-19T12:00:00Z', true),
				archive('older', '2026-09-18T12:00:00Z', true)
			],
			newest: 'hot',
			newestConsistent: 'cold'
		},
		{
			name: 'no consistent archive leaves that side empty',
			list: [
				archive('a', '2026-09-20T12:00:00Z', false),
				archive('b', '2026-09-19T12:00:00Z', false)
			],
			newest: 'a',
			newestConsistent: null
		},
		{
			name: 'the order of the list does not decide the answer',
			list: [
				archive('old', '2026-09-18T12:00:00Z', true),
				archive('hot', '2026-09-20T12:00:00Z', false),
				archive('cold', '2026-09-19T12:00:00Z', true)
			],
			newest: 'hot',
			newestConsistent: 'cold'
		},
		{
			name: 'equal times keep the earlier entry',
			list: [
				archive('first', '2026-09-20T12:00:00Z', true),
				archive('second', '2026-09-20T12:00:00Z', true)
			],
			newest: 'first',
			newestConsistent: 'first'
		}
	];

	for (const c of cases) {
		it(c.name, () => {
			const points = recoveryPoints(c.list);
			expect(points.newest?.id ?? null).toBe(c.newest);
			expect(points.newestConsistent?.id ?? null).toBe(c.newestConsistent);
		});
	}
});

describe('backupAge', () => {
	const now = Date.parse('2026-09-20T12:00:00Z');
	const cases: Array<[name: string, createdAt: string, want: string]> = [
		['seconds old', '2026-09-20T11:59:30Z', 'just now'],
		['a moment in the future', '2026-09-20T12:00:30Z', 'just now'],
		['one minute', '2026-09-20T11:59:00Z', '1 minute ago'],
		['under an hour', '2026-09-20T11:15:00Z', '45 minutes ago'],
		['one hour', '2026-09-20T11:00:00Z', '1 hour ago'],
		['under a day', '2026-09-19T13:00:00Z', '23 hours ago'],
		['one day', '2026-09-19T12:00:00Z', '1 day ago'],
		['several days', '2026-09-13T12:00:00Z', '7 days ago']
	];

	for (const [name, createdAt, want] of cases) {
		it(name, () => expect(backupAge(createdAt, now)).toBe(want));
	}
});
