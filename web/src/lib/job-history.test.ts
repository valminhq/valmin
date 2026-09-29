import { describe, expect, it } from 'vitest';
import type { Job, JobChange } from '$lib/api/types';
import {
	ago,
	changeLine,
	formatDuration,
	jobActor,
	jobDuration,
	jobLabel,
	jobOutcome,
	nextAction
} from './job-history';

const update: JobChange = {
	action: 'update',
	full_name: 'Foo-Bar',
	from_version: '1.2.0',
	to_version: '1.3.0'
};

describe('job labels', () => {
	const cases: Array<[name: string, kind: string, expected: string]> = [
		['a two-word kind', 'game_update', 'Game update'],
		['a one-word kind', 'start', 'Start'],
		['a kind this build does not know', 'some_new_kind', 'Some new kind'],
		['an empty kind', '', 'Operation']
	];
	it.each(cases)('%s', (_, kind, expected) => {
		expect(jobLabel(kind)).toBe(expected);
	});

	it('calls a mod install that only moves packages to newer versions an update', () => {
		expect(jobLabel('mod_install', [update, { ...update, full_name: 'Baz-Qux' }])).toBe(
			'Mod update'
		);
	});

	it('keeps an install an install, with or without the changes it carries', () => {
		expect(jobLabel('mod_install')).toBe('Mod install');
		expect(jobLabel('mod_install', [])).toBe('Mod install');
		expect(jobLabel('mod_install', [{ action: 'install', full_name: 'Foo-Bar' }])).toBe(
			'Mod install'
		);
	});
});

describe('who started a job', () => {
	const cases: Array<[name: string, job: Pick<Job, 'requested_by_name' | 'scheduled'>, string]> = [
		['a person', { requested_by_name: 'ada' }, 'ada'],
		['a schedule', { scheduled: true }, 'Schedule'],
		['a schedule over a stale name', { scheduled: true, requested_by_name: 'ada' }, 'Schedule'],
		['nobody the panel can name', {}, 'Panel'],
		['an empty name', { requested_by_name: '' }, 'Panel']
	];
	it.each(cases)('%s', (_, job, expected) => {
		expect(jobActor(job)).toBe(expected);
	});
});

describe('job outcomes', () => {
	it('reads work still under way as in progress, and a finished job as itself', () => {
		expect(jobOutcome('queued')).toBe('requested');
		expect(jobOutcome('running')).toBe('requested');
		expect(jobOutcome('succeeded')).toBe('succeeded');
		expect(jobOutcome('failed')).toBe('failed');
		expect(jobOutcome('cancelled')).toBe('cancelled');
	});
});

describe('durations', () => {
	const cases: Array<[name: string, ms: number, expected: string]> = [
		['under a second', 400, 'under 1s'],
		['seconds', 8_000, '8s'],
		['minutes and seconds', 125_000, '2m 5s'],
		['whole minutes', 120_000, '2m'],
		['hours and minutes', 3_780_000, '1h 3m'],
		['whole hours', 7_200_000, '2h']
	];
	it.each(cases)('%s', (_, ms, expected) => {
		expect(formatDuration(ms)).toBe(expected);
	});

	it('is the finish minus the start', () => {
		expect(
			jobDuration({ started_at: '2026-09-20T12:00:00Z', finished_at: '2026-09-20T12:02:05Z' })
		).toBe('2m 5s');
	});

	it('is unknown until the job has started and finished, or when the clock ran backwards', () => {
		expect(jobDuration({ started_at: '2026-09-20T12:00:00Z' })).toBeNull();
		expect(jobDuration({ finished_at: '2026-09-20T12:00:00Z' })).toBeNull();
		expect(
			jobDuration({ started_at: '2026-09-20T12:00:05Z', finished_at: '2026-09-20T12:00:00Z' })
		).toBeNull();
	});
});

describe('relative times', () => {
	const now = Date.parse('2026-09-20T12:00:00Z');
	const cases: Array<[name: string, at: string, expected: string]> = [
		['seconds ago', '2026-09-20T11:59:40Z', 'just now'],
		['a future time', '2026-09-20T12:05:00Z', 'just now'],
		['minutes', '2026-09-20T11:55:00Z', '5 minutes ago'],
		['hours', '2026-09-20T09:00:00Z', '3 hours ago'],
		['yesterday', '2026-09-19T11:00:00Z', 'yesterday'],
		['days', '2026-09-15T12:00:00Z', '5 days ago'],
		['an unreadable time', 'not a time', '']
	];
	it.each(cases)('%s', (_, at, expected) => {
		expect(ago(at, now)).toBe(expected);
	});
});

describe('package changes', () => {
	const cases: Array<[name: string, change: JobChange, done: boolean, expected: string]> = [
		['an update, done', update, true, 'Updated Foo-Bar 1.2.0 to 1.3.0'],
		['an update, asked', update, false, 'Update Foo-Bar 1.2.0 to 1.3.0'],
		[
			'an update that does not say where from',
			{ action: 'update', full_name: 'Foo-Bar', to_version: '1.3.0' },
			true,
			'Updated Foo-Bar to 1.3.0'
		],
		[
			'an install with a version',
			{ action: 'install', full_name: 'Foo-Bar', to_version: '1.3.0' },
			true,
			'Installed Foo-Bar 1.3.0'
		],
		[
			'an install with no version',
			{ action: 'install', full_name: 'Foo-Bar' },
			true,
			'Installed Foo-Bar'
		],
		['an uninstall', { action: 'uninstall', full_name: 'Foo-Bar' }, true, 'Uninstalled Foo-Bar'],
		['a disable', { action: 'disable', full_name: 'Foo-Bar' }, false, 'Disable Foo-Bar'],
		['an enable', { action: 'enable', full_name: 'Foo-Bar' }, true, 'Enabled Foo-Bar']
	];
	it.each(cases)('%s', (_, change, done, expected) => {
		expect(changeLine(change, done)).toBe(expected);
	});
});

describe('what to do after a failure', () => {
	const cases: Array<[kind: string, section: string]> = [
		['start', 'console'],
		['restart', 'console'],
		['mod_install', 'mods'],
		['mod_uninstall', 'mods'],
		['mod_toggle', 'mods'],
		['backup', 'backups'],
		['restore', 'backups'],
		['setup_save', 'setups'],
		['setup_restore', 'setups'],
		['setup_delete', 'setups'],
		['game_update', 'update']
	];
	it.each(cases)('points a failed %s at %s', (kind, section) => {
		expect(nextAction({ kind, status: 'failed' })?.section).toBe(section);
	});

	it('points nowhere for a job that did not fail, or a kind with no screen', () => {
		expect(nextAction({ kind: 'start', status: 'succeeded' })).toBeNull();
		expect(nextAction({ kind: 'start', status: 'cancelled' })).toBeNull();
		expect(nextAction({ kind: 'world_import', status: 'failed' })).toBeNull();
		expect(nextAction({ kind: 'constructor', status: 'failed' })).toBeNull();
	});
});
