import { describe, expect, it } from 'vitest';
import type { Backup } from '#lib/api/backups.js';
import { PLAYER_HISTORY_MS, playersRetained, worldChange } from '#lib/backup-compare.js';

function archive(id: string, createdAt: string, extra: Partial<Backup> = {}): Backup {
	return {
		id,
		instance_id: 'one',
		size_bytes: 1,
		sha256: '',
		world_name: 'World',
		trigger: 'manual',
		consistent: true,
		created_at: createdAt,
		filename: `${id}.tar.gz`,
		prunes_next: false,
		world_bytes: null,
		world_attention: [],
		...extra
	};
}

describe('worldChange', () => {
	const newest = archive('c', '2026-09-22T12:00:00Z', {
		world_bytes: 300,
		world_attention: [
			{ code: 'world_shrunk', message: 'shrunk' },
			{ code: 'chunk_missing', message: 'missing' }
		]
	});
	const middle = archive('b', '2026-09-21T12:00:00Z');
	const oldest = archive('a', '2026-09-20T12:00:00Z', {
		world_bytes: 500,
		world_attention: [{ code: 'world_shrunk', message: 'shrunk' }]
	});
	const list = [newest, middle, oldest];

	it('orders the pair whichever way it was picked', () => {
		for (const [x, y] of [
			[newest, oldest],
			[oldest, newest]
		]) {
			const change = worldChange(x, y, list);
			expect(change.older.id).toBe('a');
			expect(change.newer.id).toBe('c');
		}
	});

	it('reads the size delta, new findings and the archives in between', () => {
		const change = worldChange(oldest, newest, list);
		expect(change.deltaBytes).toBe(-200);
		expect(change.newFindings.map((f) => f.code)).toEqual(['chunk_missing']);
		expect(change.between.map((b) => b.id)).toEqual(['b']);
	});

	it('leaves the delta unknown when either world was never measured', () => {
		expect(worldChange(middle, newest, list).deltaBytes).toBeNull();
	});
});

describe('playersRetained', () => {
	const now = Date.parse('2026-10-30T00:00:00Z');
	const iso = (ms: number) => new Date(ms).toISOString();
	const cases: Array<{ name: string; from: number; to: number; want: boolean }> = [
		{ name: 'a recent window', from: now - 86_400_000, to: now, want: true },
		{ name: 'an empty window', from: now - 86_400_000, to: now - 86_400_000, want: false },
		{ name: 'exactly the retention', from: now - PLAYER_HISTORY_MS, to: now, want: true },
		{
			name: 'a start past retention',
			from: now - PLAYER_HISTORY_MS - 1,
			to: now - 86_400_000,
			want: false
		},
		{
			name: 'a window longer than the daemon serves',
			from: now - 31 * 86_400_000,
			to: now,
			want: false
		}
	];
	for (const c of cases) {
		it(c.name, () => expect(playersRetained(iso(c.from), iso(c.to), now)).toBe(c.want));
	}
});
