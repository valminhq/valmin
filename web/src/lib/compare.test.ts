import { describe, expect, it } from 'vitest';
import type { ManifestLaunch } from '$lib/api/manifest';
import {
	compareConfigs,
	compareMods,
	compareSettings,
	modChanges,
	pair,
	type ModState,
	type Row
} from './compare';

function launch(overrides: Partial<ManifestLaunch> = {}): ManifestLaunch {
	return {
		server_name: 'My Server',
		world_name: 'MyWorld',
		public: false,
		crossplay: false,
		mem_limit_mb: 4096,
		cpu_limit: null,
		backup_keep_cold: 7,
		backup_keep_hot: 3,
		backup_on_restart: false,
		...overrides
	};
}

const mod = (
	full_name: string,
	version = '1.0.0',
	enabled = true,
	source: ModState['source'] = 'thunderstore'
): ModState => ({ full_name, version, enabled, source });

describe('pair', () => {
	it('pairs by key, sorted, and classifies each row', () => {
		const rows = pair(
			[
				{ k: 'b', v: 1 },
				{ k: 'a', v: 1 },
				{ k: 'c', v: 1 }
			],
			[
				{ k: 'd', v: 1 },
				{ k: 'b', v: 2 },
				{ k: 'a', v: 1 }
			],
			(item) => item.k,
			(x, y) => x.v === y.v
		);
		expect(rows.map((row) => [row.key, row.change])).toEqual([
			['a', 'same'],
			['b', 'changed'],
			['c', 'only-a'],
			['d', 'only-b']
		]);
		expect(rows[2].b).toBeNull();
		expect(rows[3].a).toBeNull();
	});

	it('is empty for two empty lists', () => {
		expect(
			pair<string>(
				[],
				[],
				(item) => item,
				() => true
			)
		).toEqual([]);
	});
});

describe('compareSettings', () => {
	it('puts the game build first and every launch field after it', () => {
		const rows = compareSettings(
			{ build: '1', launch: launch() },
			{ build: '1', launch: launch() }
		);
		expect(rows.map((row) => row.label)).toEqual([
			'Game build',
			'Server name',
			'World name',
			'List publicly',
			'Crossplay',
			'World preset',
			'Extra arguments',
			'Memory limit (MB)',
			'CPU limit (cores)',
			'Backups to keep (server stopped)',
			'Backups to keep (best-effort)',
			'Back up when this server restarts'
		]);
		expect(rows.every((row) => row.change === 'same')).toBe(true);
	});

	const cases: Array<{
		name: string;
		a: { build?: string; launch: ManifestLaunch };
		b: { build?: string; launch: ManifestLaunch };
		key: string;
		want: [a: string, b: string];
		change: 'same' | 'changed';
	}> = [
		{
			name: 'a missing build is unknown',
			a: { launch: launch() },
			b: { build: '22000000', launch: launch() },
			key: 'game_build',
			want: ['Unknown', '22000000'],
			change: 'changed'
		},
		{
			name: 'booleans read Yes and No',
			a: { launch: launch({ crossplay: true }) },
			b: { launch: launch() },
			key: 'crossplay',
			want: ['Yes', 'No'],
			change: 'changed'
		},
		{
			name: 'null is not set',
			a: { launch: launch({ cpu_limit: 2 }) },
			b: { launch: launch() },
			key: 'cpu_limit',
			want: ['2', 'Not set'],
			change: 'changed'
		},
		{
			name: 'an empty string and an absent field are both not set',
			a: { launch: launch({ preset: '' }) },
			b: { launch: launch() },
			key: 'preset',
			want: ['Not set', 'Not set'],
			change: 'same'
		},
		{
			name: 'numbers are shown as written',
			a: { launch: launch({ mem_limit_mb: 8192 }) },
			b: { launch: launch() },
			key: 'mem_limit_mb',
			want: ['8192', '4096'],
			change: 'changed'
		}
	];

	it.each(cases)('$name', ({ a, b, key, want, change }) => {
		const row = compareSettings(a, b).find((r) => r.key === key);
		expect([row?.a, row?.b]).toEqual(want);
		expect(row?.change).toBe(change);
	});

	it('flattens modifiers into one row per key, sorted', () => {
		const rows = compareSettings(
			{ launch: launch({ modifiers: { raids: 'none', combat: 'hard' } }) },
			{ launch: launch({ modifiers: { combat: 'hard', resources: 'more' } }) }
		).filter((row) => row.key.startsWith('modifiers.'));
		expect(rows).toEqual([
			{ key: 'modifiers.combat', label: 'Modifier: combat', a: 'hard', b: 'hard', change: 'same' },
			{
				key: 'modifiers.raids',
				label: 'Modifier: raids',
				a: 'none',
				b: 'Not set',
				change: 'changed'
			},
			{
				key: 'modifiers.resources',
				label: 'Modifier: resources',
				a: 'Not set',
				b: 'more',
				change: 'changed'
			}
		]);
	});

	it('labels a field it does not know by its key', () => {
		const future = { ...launch(), future_flag: true } as ManifestLaunch;
		const row = compareSettings({ launch: future }, { launch: launch() }).find(
			(r) => r.key === 'future_flag'
		);
		expect(row).toEqual({
			key: 'future_flag',
			label: 'future_flag',
			a: 'Yes',
			b: 'Not set',
			change: 'changed'
		});
	});
});

describe('compareMods', () => {
	const cases: Array<{ name: string; a: ModState[]; b: ModState[]; change: string }> = [
		{ name: 'same version and state', a: [mod('X')], b: [mod('X')], change: 'same' },
		{
			name: 'different version',
			a: [mod('X', '1.0.0')],
			b: [mod('X', '1.1.0')],
			change: 'changed'
		},
		{
			name: 'enabled on one side',
			a: [mod('X', '1.0.0', false)],
			b: [mod('X')],
			change: 'changed'
		},
		{
			name: 'same version from another registry',
			a: [mod('X')],
			b: [mod('X', '1.0.0', true, 'hexium')],
			change: 'changed'
		},
		{
			name: 'different version lock',
			a: [{ ...mod('X'), locked: true }],
			b: [{ ...mod('X'), locked: false }],
			change: 'changed'
		},
		{
			name: 'different client tag',
			a: [{ ...mod('X'), side: 'client_required' }],
			b: [{ ...mod('X'), side: 'server_only' }],
			change: 'changed'
		},
		{
			name: 'different install reason',
			a: [{ ...mod('X'), installed_as: 'explicit' }],
			b: [{ ...mod('X'), installed_as: 'dependency' }],
			change: 'changed'
		},
		{ name: 'only on the first', a: [mod('X')], b: [], change: 'only-a' },
		{ name: 'only on the second', a: [], b: [mod('X')], change: 'only-b' }
	];

	it.each(cases)('$name', ({ a, b, change }) => {
		expect(compareMods(a, b).map((row) => row.change)).toEqual([change]);
	});
});

describe('compareConfigs', () => {
	it('keeps both contents and marks a differing file changed', () => {
		const rows = compareConfigs(
			[
				{ file: 'a.cfg', content: 'x = 1\n' },
				{ file: 'b.cfg', content: 'same\n' }
			],
			[
				{ file: 'a.cfg', content: 'x = 2\n' },
				{ file: 'b.cfg', content: 'same\n' },
				{ file: 'c.cfg', content: 'new\n' }
			]
		);
		expect(rows.map((row) => [row.key, row.change])).toEqual([
			['a.cfg', 'changed'],
			['b.cfg', 'same'],
			['c.cfg', 'only-b']
		]);
		expect([rows[0].a?.content, rows[0].b?.content]).toEqual(['x = 1\n', 'x = 2\n']);
	});
});

describe('modChanges', () => {
	const row = (a: ModState | null, b: ModState | null): Row<ModState> => ({
		key: 'X',
		a,
		b,
		change: 'changed'
	});

	const cases: Array<{ name: string; row: Row<ModState>; want: string[] }> = [
		{ name: 'only on the first', row: row(mod('X'), null), want: ['Only on Live'] },
		{ name: 'only on the second', row: row(null, mod('X')), want: ['Only on Test'] },
		{
			name: 'different version',
			row: row(mod('X', '1.0.0'), mod('X', '2.0.0')),
			want: ['Different version']
		},
		{
			name: 'enabled only on the second',
			row: row(mod('X', '1.0.0', false), mod('X')),
			want: ['Enabled only on Test']
		},
		{
			name: 'another registry',
			row: row(mod('X'), mod('X', '1.0.0', true, 'hexium')),
			want: ['Different registry']
		},
		{
			name: 'lock and client tag differ',
			row: row(
				{ ...mod('X'), locked: true, side: 'client_required' },
				{ ...mod('X'), locked: false, side: 'server_only' }
			),
			want: ['Different version lock', 'Different client tag']
		},
		{
			name: 'install reason differs',
			row: row(
				{ ...mod('X'), installed_as: 'explicit' },
				{ ...mod('X'), installed_as: 'dependency' }
			),
			want: ['Different install reason']
		},
		{
			name: 'both differences at once',
			row: row(mod('X', '1.0.0'), mod('X', '2.0.0', false)),
			want: ['Different version', 'Enabled only on Live']
		}
	];

	it.each(cases)('$name', ({ row, want }) => {
		expect(modChanges(row, 'Live', 'Test')).toEqual(want);
	});
});
