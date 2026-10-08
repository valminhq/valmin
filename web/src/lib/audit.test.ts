import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AuditEntry, AuditOutcome } from '$lib/api/admin';
import {
	actorName,
	changes,
	dayRange,
	describe as summarise,
	label,
	outcomeLabel,
	outcomeVariant,
	serverName
} from './audit';

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
	return {
		id: 'a-1',
		user_id: 'u-1',
		actor: 'Alex',
		instance_id: 'inst-a',
		instance: 'Viking World',
		action: 'instances.start',
		detail: '{}',
		ip: '203.0.113.7',
		outcome: 'succeeded',
		job_id: null,
		job_error: null,
		created_at: '2026-09-20T12:00:00Z',
		...overrides
	};
}

const json = JSON.stringify;

describe('summaries of recorded actions', () => {
	const cases: Array<[name: string, action: string, detail: unknown, expected: string]> = [
		['start', 'instances.start', {}, 'Alex started Viking World'],
		['stop', 'instances.stop', {}, 'Alex stopped Viking World'],
		['restart', 'instances.restart', {}, 'Alex restarted Viking World'],
		[
			'delete keeping worlds',
			'instances.delete',
			{ keep_worlds: true },
			'Alex deleted Viking World and kept its worlds'
		],
		[
			'delete with worlds',
			'instances.delete',
			{ keep_worlds: false },
			'Alex deleted Viking World and its worlds'
		],
		['delete without the flag', 'instances.delete', {}, 'Alex deleted Viking World'],
		[
			'create from scratch',
			'instances.create',
			{ name: 'Fresh', source: 'new' },
			'Alex created Fresh'
		],
		[
			'create from a manifest',
			'instances.create',
			{ name: 'Fresh', source: 'manifest' },
			'Alex created Fresh from a manifest'
		],
		['game update', 'instances.game.update', {}, 'Alex updated the game on Viking World'],
		['backup create', 'instances.backups.create', {}, 'Alex created a backup on Viking World'],
		[
			'backup restore',
			'instances.backups.restore',
			{ backup_id: 'b-1' },
			'Alex restored a backup on Viking World'
		],
		[
			'backup delete',
			'instances.backups.delete',
			{ backup_id: 'b-1' },
			'Alex deleted a backup on Viking World'
		],
		[
			'setup save',
			'instances.setups.save',
			{ name: 'Working before update' },
			'Alex saved setup Working before update on Viking World'
		],
		[
			'setup restore',
			'instances.setups.restore',
			{ name: 'Working before update' },
			'Alex restored setup Working before update on Viking World'
		],
		[
			'setup delete',
			'instances.setups.delete',
			{ name: 'Working before update' },
			'Alex deleted setup Working before update on Viking World'
		],
		[
			'world import',
			'instances.worlds.import',
			{ world: 'Midgard' },
			'Alex imported world Midgard on Viking World'
		],
		[
			'world restore',
			'instances.worlds.restore',
			{ world: 'Midgard' },
			'Alex restored world Midgard on Viking World'
		],
		[
			'world delete',
			'instances.worlds.delete',
			{ world: 'Midgard' },
			'Alex deleted world Midgard on Viking World'
		],
		[
			'resume',
			'instances.operation.resume',
			{},
			'Alex resumed an interrupted operation on Viking World'
		],
		[
			'abandon',
			'instances.operation.abandon',
			{},
			'Alex abandoned an interrupted operation on Viking World'
		],
		[
			'job cancel',
			'jobs.cancel',
			{ job_id: 'j-1', kind: 'game_update' },
			'Alex cancelled a game update job on Viking World'
		],
		[
			'mod version moved down',
			'instances.mods.install',
			{ full_name: 'ExampleMod', from: '2.4.0', to: '2.3.1', source: 'thunderstore' },
			'Alex changed ExampleMod from 2.4.0 to 2.3.1 on Viking World'
		],
		[
			'mod version moved up',
			'instances.mods.install',
			{ full_name: 'ExampleMod', from: '2.3.1', to: '2.4.0', source: 'thunderstore' },
			'Alex changed ExampleMod from 2.3.1 to 2.4.0 on Viking World'
		],
		[
			'first install of a mod',
			'instances.mods.install',
			{ full_name: 'ExampleMod', to: '2.3.1', source: 'thunderstore' },
			'Alex installed ExampleMod 2.3.1 on Viking World'
		],
		[
			'update of one mod',
			'instances.mods.update',
			{ packages: [{ full_name: 'ExampleMod', from: '1.0.0', to: '1.1.0' }] },
			'Alex updated ExampleMod from 1.0.0 to 1.1.0 on Viking World'
		],
		[
			'update of several mods',
			'instances.mods.update',
			{
				packages: [
					{ full_name: 'A', from: '1', to: '2' },
					{ full_name: 'B', from: '1', to: '2' }
				]
			},
			'Alex updated 2 mods on Viking World'
		],
		['update of no listed mods', 'instances.mods.update', {}, 'Alex updated mods on Viking World'],
		[
			'uninstall of one mod',
			'instances.mods.uninstall',
			{ full_names: ['ExampleMod'] },
			'Alex uninstalled ExampleMod on Viking World'
		],
		[
			'uninstall of several mods',
			'instances.mods.uninstall',
			{ full_names: ['A', 'B', 'C'] },
			'Alex uninstalled 3 mods on Viking World'
		],
		[
			'enable',
			'instances.mods.enable',
			{ full_name: 'ExampleMod' },
			'Alex enabled ExampleMod on Viking World'
		],
		[
			'disable',
			'instances.mods.disable',
			{ full_name: 'ExampleMod' },
			'Alex disabled ExampleMod on Viking World'
		],
		[
			'lock',
			'instances.mods.lock',
			{ full_name: 'ExampleMod', version: '2.3.1' },
			'Alex locked ExampleMod at 2.3.1 on Viking World'
		],
		[
			'unlock',
			'instances.mods.unlock',
			{ full_name: 'ExampleMod', version: '2.3.1' },
			'Alex unlocked ExampleMod on Viking World'
		],
		[
			'one setting',
			'instances.settings.update',
			{ changes: [{ field: 'mem_limit_mb', from: 4096, to: 8192 }] },
			'Alex changed mem_limit_mb from 4096 to 8192 on Viking World'
		],
		[
			'a secret setting',
			'instances.settings.update',
			{ changes: [{ field: 'password', secret: true }] },
			'Alex changed the password on Viking World'
		],
		[
			'two settings',
			'instances.settings.update',
			{
				changes: [
					{ field: 'public', from: false, to: true },
					{ field: 'world_name', from: 'Old', to: 'New' }
				]
			},
			'Alex changed public from false to true and world_name from Old to New on Viking World'
		],
		[
			'three settings',
			'instances.settings.update',
			{
				changes: [
					{ field: 'a', from: 1, to: 2 },
					{ field: 'b', from: 3, to: 4 },
					{ field: 'c', from: 5, to: 6 }
				]
			},
			'Alex changed a from 1 to 2, b from 3 to 4 and 1 more on Viking World'
		],
		[
			'a config value',
			'instances.configs.write',
			{
				file: 'example.cfg',
				bytes: 120,
				raw: false,
				changes: [{ field: 'Section.Key', from: '1', to: '2' }]
			},
			'Alex edited example.cfg on Viking World: Section.Key 1 → 2'
		],
		[
			'a secret config value',
			'instances.configs.write',
			{ file: 'example.cfg', changes: [{ field: 'Section.Token', secret: true }] },
			'Alex edited example.cfg on Viking World: Section.Token (value hidden)'
		],
		[
			'a config write with no listed changes',
			'instances.configs.write',
			{ file: 'example.cfg', bytes: 120, raw: true },
			'Alex edited example.cfg on Viking World'
		],
		[
			'a config file deleted',
			'instances.configs.delete',
			{ file: 'example.cfg', installed_mods: [] },
			'Alex deleted example.cfg on Viking World'
		],
		[
			'a command',
			'instances.commands.send',
			{ channel: 'rcon', command: 'save' },
			"Alex sent command 'save' on Viking World"
		],
		[
			'a new schedule',
			'schedules.create',
			{ kind: 'backup', cron: '0 4 * * *', enabled: true },
			'Alex created a backup schedule (0 4 * * *) on Viking World'
		],
		[
			'a new schedule that is off',
			'schedules.create',
			{ kind: 'game_update', cron: '0 5 * * 1', enabled: false },
			'Alex created a game update schedule (0 5 * * 1), disabled on Viking World'
		],
		[
			'a changed schedule',
			'schedules.update',
			{ kind: 'backup', changes: [{ field: 'cron', from: '0 4 * * *', to: '0 5 * * *' }] },
			'Alex changed the backup schedule on Viking World: cron from 0 4 * * * to 0 5 * * *'
		],
		[
			'a removed schedule',
			'schedules.delete',
			{ kind: 'backup', cron: '0 4 * * *' },
			'Alex deleted the backup schedule (0 4 * * *) on Viking World'
		]
	];

	it.each(cases)('says %s', (_name, action, detail, expected) => {
		expect(summarise(entry({ action, detail: json(detail) }))).toBe(expected);
	});

	it('reads a missing detail as an empty one for a known action', () => {
		expect(summarise(entry({ detail: null }))).toBe('Alex started Viking World');
	});

	const subjects: Array<[name: string, overrides: Partial<AuditEntry>, expected: string]> = [
		['an actor who was deleted', { actor: null }, 'Deleted user started Viking World'],
		[
			'an action by the panel itself',
			{ actor: null, user_id: null },
			'The panel started Viking World'
		],
		['a server that is gone', { instance: null }, 'Alex started a deleted server'],
		['an entry about no server', { instance: null, instance_id: null }, 'Alex started a server']
	];

	it.each(subjects)('names %s', (_name, overrides, expected) => {
		expect(summarise(entry(overrides))).toBe(expected);
	});

	it('trails "on <server>" with the name it was recorded under, or a deleted one', () => {
		expect(summarise(entry({ action: 'instances.game.update', instance: null }))).toBe(
			'Alex updated the game on a deleted server'
		);
		expect(
			summarise(entry({ action: 'instances.game.update', instance: null, instance_id: null }))
		).toBe('Alex updated the game');
	});
});

describe('entries the summary cannot read', () => {
	const cases: Array<[name: string, overrides: Partial<AuditEntry>, expected: string]> = [
		[
			'an action it does not know, with detail',
			{ action: 'widgets.frobnicate', detail: '{"a":1}' },
			'Alex: Widgets frobnicate: {"a":1}'
		],
		[
			'an action it does not know, without detail',
			{ action: 'widgets.frobnicate', detail: null },
			'Alex: Widgets frobnicate'
		],
		[
			'an action with underscores in its name',
			{ action: 'widgets.hot_swap', detail: null },
			'Alex: Widgets hot swap'
		],
		[
			'an action named like an object method',
			{ action: 'constructor', detail: '{}' },
			'Alex: Constructor: {}'
		],
		[
			'free text where JSON is recorded',
			{ action: 'instances.configs.write', detail: 'example.cfg, 12 bytes' },
			'Alex: Config file edited: example.cfg, 12 bytes'
		],
		[
			'a bare string',
			{ action: 'instances.start', detail: '"started"' },
			'Alex: Server started: "started"'
		],
		['a number', { action: 'instances.start', detail: '42' }, 'Alex: Server started: 42'],
		['an array', { action: 'instances.start', detail: '[1,2]' }, 'Alex: Server started: [1,2]'],
		[
			'the JSON literal null',
			{ action: 'instances.start', detail: 'null' },
			'Alex: Server started: null'
		],
		[
			'truncated JSON',
			{ action: 'instances.start', detail: '{"a":' },
			'Alex: Server started: {"a":'
		],
		[
			'an old-format entry of an existing action',
			{ action: 'users.password.reset', detail: '{"target":"u-2"}' },
			'Alex: Users password reset: {"target":"u-2"}'
		]
	];

	it.each(cases)('falls back for %s', (_name, overrides, expected) => {
		expect(summarise(entry(overrides))).toBe(expected);
	});

	it('clips a long raw detail', () => {
		const long = 'x'.repeat(300);
		const said = summarise(entry({ action: 'widgets.frobnicate', detail: long }));
		expect(said).toBe(`Alex: Widgets frobnicate: ${'x'.repeat(120)}…`);
	});

	const malformed: Array<[name: string, action: string, detail: unknown, expected: string]> = [
		[
			'changes that are not a list',
			'instances.settings.update',
			{ changes: 'nope' },
			'Alex changed settings on Viking World'
		],
		[
			'changes that are not objects',
			'instances.settings.update',
			{ changes: [1, null, 'x'] },
			'Alex changed settings on Viking World'
		],
		[
			'a package that is null',
			'instances.mods.update',
			{ packages: [null] },
			'Alex updated mods on Viking World'
		],
		['a mod with no name', 'instances.mods.enable', {}, 'Alex enabled a mod on Viking World'],
		[
			'a command with no text',
			'instances.commands.send',
			{ channel: 'rcon' },
			'Alex sent a command on Viking World'
		],
		[
			'a job cancel with no kind',
			'jobs.cancel',
			{ job_id: 'j-1' },
			'Alex cancelled a background job on Viking World'
		],
		[
			'a field that is an object',
			'instances.settings.update',
			{ changes: [{ field: { a: 1 }, from: {}, to: [] }] },
			'Alex changed (unnamed) from {} to [] on Viking World'
		]
	];

	it.each(malformed)('does not throw on %s', (_name, action, detail, expected) => {
		expect(summarise(entry({ action, detail: json(detail) }))).toBe(expected);
	});
});

describe('the changes an entry recorded', () => {
	const cases: Array<[name: string, detail: string | null, expected: unknown]> = [
		[
			'a settings diff',
			json({ changes: [{ field: 'mem_limit_mb', from: 4096, to: 8192 }] }),
			[{ field: 'mem_limit_mb', from: '4096', to: '8192', secret: false }]
		],
		[
			'a secret, whose values are not recorded',
			json({ changes: [{ field: 'password', secret: true }] }),
			[{ field: 'password', from: '', to: '', secret: true }]
		],
		[
			'a secret that carried values anyway',
			json({ changes: [{ field: 'password', from: 'a', to: 'b', secret: true }] }),
			[{ field: 'password', from: '', to: '', secret: true }]
		],
		[
			'a key that was added',
			json({ changes: [{ field: 'S.K', to: '1' }] }),
			[{ field: 'S.K', from: '(none)', to: '1', secret: false }]
		],
		[
			'a key that was removed',
			json({ changes: [{ field: 'S.K', from: '1' }] }),
			[{ field: 'S.K', from: '1', to: '(none)', secret: false }]
		],
		[
			'values that are empty, false or zero',
			json({
				changes: [
					{ field: 'a', from: '', to: 'x' },
					{ field: 'b', from: true, to: false },
					{ field: 'c', from: 1, to: 0 }
				]
			}),
			[
				{ field: 'a', from: '(empty)', to: 'x', secret: false },
				{ field: 'b', from: 'true', to: 'false', secret: false },
				{ field: 'c', from: '1', to: '0', secret: false }
			]
		],
		[
			'a modpack update',
			json({
				packages: [
					{ full_name: 'A', from: '1', to: '2' },
					{ full_name: 'B', to: '3' }
				]
			}),
			[
				{ field: 'A', from: '1', to: '2', secret: false },
				{ field: 'B', from: '(none)', to: '3', secret: false }
			]
		],
		[
			'a mod moved between versions',
			json({ full_name: 'ExampleMod', from: '2.4.0', to: '2.3.1', source: 'thunderstore' }),
			[{ field: 'ExampleMod', from: '2.4.0', to: '2.3.1', secret: false }]
		],
		[
			'a first install',
			json({ full_name: 'ExampleMod', to: '2.3.1' }),
			[{ field: 'ExampleMod', from: '(none)', to: '2.3.1', secret: false }]
		],
		['a lock, which changes no value', json({ full_name: 'ExampleMod', version: '1' }), []],
		['a detail with no changes', '{}', []],
		['free text', 'example.cfg, 12 bytes', []],
		['a bare string', '"text"', []],
		['an array', '[1]', []],
		['no detail', null, []],
		['an empty detail', '', []],
		['changes that are not a list', json({ changes: 'x' }), []]
	];

	it.each(cases)('reads %s', (_name, detail, expected) => {
		expect(changes(detail)).toEqual(expected);
	});
});

describe('action labels', () => {
	const cases: Array<[action: string, expected: string]> = [
		['instances.mods.install', 'Mod installed'],
		['schedules.delete', 'Schedule deleted'],
		['users.password.reset', 'Users password reset'],
		['users.password.change', 'Users password changed'],
		['instance.adopt', 'Instance adopt'],
		['instances.players.banned_players.write', 'Instances players banned players write'],
		['', '']
	];

	it.each(cases)('names %s', (action, expected) => {
		expect(label(action)).toBe(expected);
	});
});

describe('outcomes', () => {
	const cases: Array<[AuditOutcome | null, string | null, string]> = [
		['requested', 'In progress', 'outline'],
		['succeeded', 'Completed', 'secondary'],
		['failed', 'Failed', 'destructive'],
		['cancelled', 'Cancelled', 'outline'],
		[null, null, 'outline']
	];

	it.each(cases)('labels %s', (outcome, text, variant) => {
		expect(outcomeLabel(outcome)).toBe(text);
		expect(outcomeVariant(outcome)).toBe(variant);
	});

	it('passes an outcome it does not know through as it is', () => {
		expect(outcomeLabel('paused' as AuditOutcome)).toBe('paused');
	});
});

describe('who and where', () => {
	it('names the actor, or what became of them', () => {
		expect(actorName({ actor: 'Alex', user_id: 'u-1' })).toBe('Alex');
		expect(actorName({ actor: null, user_id: 'u-1' })).toBe('deleted user');
		expect(actorName({ actor: null, user_id: null })).toBe('the panel');
	});

	it('names the server, or says it is gone, or names none', () => {
		expect(serverName({ instance: 'Viking World', instance_id: 'i' })).toBe('Viking World');
		expect(serverName({ instance: null, instance_id: 'i' })).toBe('deleted server');
		expect(serverName({ instance: null, instance_id: null })).toBeNull();
	});
});

describe('the local window for a range of days', () => {
	beforeEach(() => vi.stubEnv('TZ', 'UTC'));
	afterEach(() => vi.unstubAllEnvs());
	const cases: Array<[name: string, from: string, to: string, expected: unknown]> = [
		[
			'both days, the end day included',
			'2026-09-01',
			'2026-09-30',
			{ since: '2026-09-01T00:00:00Z', until: '2026-10-01T00:00:00Z' }
		],
		[
			'a single day',
			'2026-09-15',
			'2026-09-15',
			{ since: '2026-09-15T00:00:00Z', until: '2026-09-16T00:00:00Z' }
		],
		[
			'the end of a year',
			'2026-12-31',
			'2026-12-31',
			{ since: '2026-12-31T00:00:00Z', until: '2027-01-01T00:00:00Z' }
		],
		[
			'a leap day',
			'2028-02-28',
			'2028-02-29',
			{ since: '2028-02-28T00:00:00Z', until: '2028-03-01T00:00:00Z' }
		],
		['only a start', '2026-09-01', '', { since: '2026-09-01T00:00:00Z', until: undefined }],
		['only an end', '', '2026-09-01', { since: undefined, until: '2026-09-02T00:00:00Z' }],
		['neither', '', '', { since: undefined, until: undefined }],
		['days that are not dates', 'soon', '2026-13-45', { since: undefined, until: undefined }],
		['a time instead of a day', '2026-09-01T10:00', '', { since: undefined, until: undefined }]
	];

	it.each(cases)('covers %s', (_name, from, to, expected) => {
		expect(dayRange(from, to)).toEqual(expected);
	});

	it('uses local midnight across a daylight saving change', () => {
		vi.stubEnv('TZ', 'Europe/Kyiv');
		expect(dayRange('2026-03-29', '2026-03-29')).toEqual({
			since: '2026-03-28T22:00:00Z',
			until: '2026-03-29T21:00:00Z'
		});
	});
});
