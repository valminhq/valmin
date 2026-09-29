import { describe, expect, it } from 'vitest';
import { actions } from '$lib/api/instances';
import { canCompare, loginWithReturn, returnPath, serverSections, switchServerPath } from './nav';

const at = (href: string) => new URL(href, 'https://panel.example');

describe('returnPath', () => {
	it('returns a same-origin path unchanged, query included', () => {
		expect(returnPath(at('/login?next=%2Finstances%2Fabc%2Fbackups'))).toBe(
			'/instances/abc/backups'
		);
		expect(returnPath(at('/login?next=%2Fadmin%2Faudit%3Fkind%3Dstart'))).toBe(
			'/admin/audit?kind=start'
		);
	});

	it('falls back to the server list when there is nothing to return to', () => {
		expect(returnPath(at('/login'))).toBe('/');
		expect(returnPath(at('/login?next='))).toBe('/');
	});

	it('refuses any destination that leaves this origin', () => {
		for (const next of [
			'https://evil.example/steal',
			'//evil.example/steal',
			'/\\evil.example',
			'http://panel.example/instances',
			'javascript:alert(1)',
			'instances/abc'
		]) {
			expect(returnPath(at(`/login?next=${encodeURIComponent(next)}`)), next).toBe('/');
		}
	});
});

describe('loginWithReturn', () => {
	it('carries the path being left, with its query', () => {
		expect(loginWithReturn('/login', at('/instances/abc/backups'))).toBe(
			'/login?next=%2Finstances%2Fabc%2Fbackups'
		);
		expect(loginWithReturn('/login', at('/admin/audit?kind=start'))).toBe(
			'/login?next=%2Fadmin%2Faudit%3Fkind%3Dstart'
		);
	});

	it('adds nothing when the destination is already the default', () => {
		expect(loginWithReturn('/login', at('/'))).toBe('/login');
	});
});

describe('serverSections', () => {
	it.each([
		{ held: [], tabs: ['Overview', 'Server settings'] },
		{ held: [actions.configRead], tabs: ['Overview', 'Mod configuration', 'Server settings'] },
		{ held: [actions.restart], tabs: ['Overview', 'Maintenance', 'Server settings'] },
		{
			held: [
				actions.grantsManage,
				actions.playersManage,
				actions.configRead,
				actions.modsList,
				actions.backupsList,
				actions.backupsCreate
			],
			tabs: [
				'Overview',
				'Backups',
				'Maintenance',
				'Mods',
				'Mod configuration',
				'Player access',
				'Panel access',
				'Server settings'
			]
		}
	])('lists the tabs the held actions reach, in order ($held)', ({ held, tabs }) => {
		expect(serverSections(held).map((section) => section.label)).toEqual(tabs);
	});

	it('never lists Compare as a tab', () => {
		const held = [actions.settings, actions.modsList, actions.configRead];
		expect(serverSections(held).map((section) => section.segment)).not.toContain('compare');
	});
});

describe('canCompare', () => {
	it.each([
		{ held: [actions.settings, actions.modsList, actions.configRead], can: true },
		{ held: [actions.modsList, actions.configRead], can: false },
		{ held: [actions.settings, actions.configRead], can: false },
		{ held: [actions.settings, actions.modsList], can: false },
		{ held: [], can: false }
	])('needs settings, mods and configuration reads ($held)', ({ held, can }) => {
		expect(canCompare(held)).toBe(can);
	});
});

describe('switchServerPath', () => {
	const everything = [
		actions.backupsList,
		actions.backupsCreate,
		actions.modsList,
		actions.configRead,
		actions.playersManage,
		actions.grantsManage,
		actions.settings
	];
	const readOnly = [actions.view];

	it.each([
		{ name: 'overview', from: '/instances/a', held: readOnly, to: '/instances/b' },
		{ name: 'overview, trailing slash', from: '/instances/a/', held: readOnly, to: '/instances/b' },
		{ name: 'backups', from: '/instances/a/backups', held: everything, to: '/instances/b/backups' },
		{
			name: 'a section the target does not offer',
			from: '/instances/a/backups',
			held: readOnly,
			to: '/instances/b'
		},
		{
			name: 'a nested file page keeps only its section',
			from: '/instances/a/configs/plugin.cfg',
			held: everything,
			to: '/instances/b/configs'
		},
		{
			name: 'a nested file page the target cannot read',
			from: '/instances/a/configs/plugin.cfg',
			held: [actions.modsList],
			to: '/instances/b'
		},
		{
			name: 'the section that is always offered',
			from: '/instances/a/settings',
			held: readOnly,
			to: '/instances/b/settings'
		},
		{
			name: 'compare, permitted on the target',
			from: '/instances/a/compare',
			held: everything,
			to: '/instances/b/compare'
		},
		{
			name: 'compare, not permitted on the target',
			from: '/instances/a/compare',
			held: [actions.settings, actions.modsList],
			to: '/instances/b'
		},
		{
			name: 'a page that is not a section',
			from: '/instances/a/clone',
			held: everything,
			to: '/instances/b'
		},
		{
			name: 'a page of another server',
			from: '/instances/c/backups',
			held: everything,
			to: '/instances/b'
		},
		{
			name: 'a page outside any server',
			from: '/admin/users',
			held: everything,
			to: '/instances/b'
		}
	])('$name', ({ from, held, to }) => {
		expect(switchServerPath(from, 'a', 'b', held)).toBe(to);
	});

	it('does not treat a server id that prefixes another as the same server', () => {
		expect(switchServerPath('/instances/ab/backups', 'a', 'b', everything)).toBe('/instances/b');
	});
});
