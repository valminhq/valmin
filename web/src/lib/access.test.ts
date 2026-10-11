import { describe, expect, it } from 'vitest';
import { actionLabel, capitalise, describeAccess } from './access';

const viewer = [
	'instance.view',
	'console.read',
	'stats.read',
	'backups.list',
	'mods.list',
	'config.read'
];
const operatorExtra = [
	'instance.start',
	'instance.stop',
	'instance.restart',
	'backups.create',
	'backups.download',
	'players.manage',
	'commands.send'
];
const grantable = [
	'mods.manage',
	'config.edit',
	'config.raw',
	'backups.restore',
	'world.import',
	'instance.settings'
];

describe('action labels', () => {
	it('reads every base and grantable action as a lowercase phrase, not its identifier', () => {
		for (const action of [...viewer, ...operatorExtra, ...grantable]) {
			const label = actionLabel(action);
			expect(label, action).toMatch(/^[a-z]+( [a-z]+)+$/);
			expect(label, action).not.toBe(action);
		}
	});

	const cases: Array<[action: string, label: string]> = [
		['console.read', 'view the console'],
		['backups.create', 'create backups'],
		['instance.restart', 'restart the server'],
		['commands.send', 'send console commands'],
		['config.raw', 'edit raw configuration files'],
		['instance.settings', 'change server settings'],
		['instance.extra_args', 'instance extra args'],
		['constructor', 'constructor']
	];
	it.each(cases)('labels %s as %s', (action, label) => {
		expect(actionLabel(action)).toBe(label);
	});
});

describe('capitalise', () => {
	it('upper-cases the first letter only', () => {
		expect(capitalise('view the server')).toBe('View the server');
		expect(capitalise('')).toBe('');
	});
});

describe('access descriptions', () => {
	const cases: Array<[name: string, actions: string[], sentence: string]> = [
		[
			'the read-only set',
			viewer,
			'Can view the server, console, stats, backups, mods, and configuration.'
		],
		[
			'an operator',
			[...viewer, ...operatorExtra],
			'Can view the server, console, stats, backups, mods, and configuration; ' +
				'start, stop, and restart the server; create and download backups; ' +
				'manage players; and send console commands.'
		],
		[
			'every extra on top of viewer',
			[...viewer, ...grantable],
			'Can view the server, console, stats, backups, mods, and configuration; ' +
				'restore backups; manage mods; edit raw configuration files; import worlds; ' +
				'and change server settings.'
		],
		[
			'a few actions',
			['console.read', 'instance.restart', 'backups.create', 'backups.download'],
			'Can view the console, restart the server, and create and download backups.'
		],
		[
			'two clauses when one holds a list',
			[...viewer, 'mods.manage'],
			'Can view the server, console, stats, backups, mods, and configuration; and manage mods.'
		],
		['one action', ['instance.restart'], 'Can restart the server.'],
		['a shared object', ['instance.view', 'instance.restart'], 'Can view and restart the server.'],
		[
			'players and mods together',
			['players.manage', 'mods.manage'],
			'Can manage players and mods.'
		],
		[
			'raw editing without the form',
			['config.edit', 'config.raw'],
			'Can edit raw configuration files.'
		],
		['duplicates', ['stats.read', 'stats.read'], 'Can view stats.'],
		[
			'an unknown action after the known ones',
			['future.thing', 'console.read'],
			'Can view the console and future thing.'
		],
		['nothing', [], 'Has no access.']
	];
	it.each(cases)('describes %s', (_name, actions, sentence) => {
		expect(describeAccess(actions)).toBe(sentence);
	});

	it('does not depend on the order actions arrive in', () => {
		const all = [...viewer, ...operatorExtra, ...grantable];
		expect(describeAccess([...all].reverse())).toBe(describeAccess(all));
	});
});
