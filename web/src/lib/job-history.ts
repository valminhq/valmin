import type { AuditOutcome } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import type { Job, JobChange, JobChangeAction, JobStatus } from '$lib/api/types';

const upperFirst = (text: string) => text.charAt(0).toUpperCase() + text.slice(1);

/**
 * A readable name for a job kind: the kind in words, so a kind this build does not know still
 * reads tidily. A mod install that only moves installed packages to newer versions is an
 * update, and says so.
 */
export function jobLabel(kind: string, changes?: JobChange[]): string {
	if (kind === 'mod_install' && changes?.length && changes.every((c) => c.action === 'update')) {
		return 'Mod update';
	}
	const words = kind.replaceAll('_', ' ').trim();
	return words ? upperFirst(words) : 'Operation';
}

/** Who started a job: the schedule clock, the person, or the panel itself. */
export function jobActor(job: Pick<Job, 'requested_by_name' | 'scheduled'>): string {
	if (job.scheduled) return 'Schedule';
	return job.requested_by_name || 'Panel';
}

/** The audit outcome a job status reads as, so the history and the audit log word it alike. */
export function jobOutcome(status: JobStatus): AuditOutcome {
	return status === 'queued' || status === 'running' ? 'requested' : status;
}

/** A span of milliseconds as "8s", "2m 5s" or "1h 3m". */
export function formatDuration(ms: number): string {
	const total = Math.round(ms / 1000);
	if (total < 1) return 'under 1s';
	const hours = Math.floor(total / 3600);
	const minutes = Math.floor((total % 3600) / 60);
	const seconds = total % 60;
	if (hours) return minutes ? `${hours}h ${minutes}m` : `${hours}h`;
	if (minutes) return seconds ? `${minutes}m ${seconds}s` : `${minutes}m`;
	return `${seconds}s`;
}

/** How long a job ran, or null while it has not both started and finished. */
export function jobDuration(job: Pick<Job, 'started_at' | 'finished_at'>): string | null {
	if (!job.started_at || !job.finished_at) return null;
	const ms = Date.parse(job.finished_at) - Date.parse(job.started_at);
	return Number.isNaN(ms) || ms < 0 ? null : formatDuration(ms);
}

const relative = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });
const units: Array<[unit: Intl.RelativeTimeFormatUnit, seconds: number]> = [
	['day', 86400],
	['hour', 3600],
	['minute', 60]
];

/** How long before `now` (epoch milliseconds) a time was, in whole units: "yesterday", "3 hours ago". */
export function ago(iso: string, now: number): string {
	const seconds = Math.floor((now - Date.parse(iso)) / 1000);
	if (Number.isNaN(seconds)) return '';
	for (const [unit, size] of units) {
		if (seconds >= size) return relative.format(-Math.floor(seconds / size), unit);
	}
	return 'just now';
}

const verbs: Record<JobChangeAction, [asked: string, done: string]> = {
	install: ['Install', 'Installed'],
	update: ['Update', 'Updated'],
	uninstall: ['Uninstall', 'Uninstalled'],
	enable: ['Enable', 'Enabled'],
	disable: ['Disable', 'Disabled']
};

/** One package change as a sentence: "Updated Foo 1.2.0 to 1.3.0". `done` picks the past tense. */
export function changeLine(change: JobChange, done: boolean): string {
	const [asked, finished] = verbs[change.action] ?? [change.action, change.action];
	const verb = done ? finished : asked;
	if (change.action === 'update') {
		const from = change.from_version ? ` ${change.from_version}` : '';
		const to = change.to_version ? ` to ${change.to_version}` : '';
		return `${verb} ${change.full_name}${from}${to}`;
	}
	return `${verb} ${change.full_name}${change.to_version ? ` ${change.to_version}` : ''}`;
}

/** The part of the overview or the server's other screens a failed job points to. */
export type Section = 'console' | 'mods' | 'backups' | 'setups' | 'update';

export interface NextAction {
	label: string;
	section: Section;
	/** The action a person needs on the server for the section to be of use to them. */
	requires: string;
}

const failedStart: NextAction = {
	label: 'Check the console',
	section: 'console',
	requires: actions.consoleRead
};
const failedMods: NextAction = { label: 'Open mods', section: 'mods', requires: actions.modsList };
const failedBackups: NextAction = {
	label: 'Open backups',
	section: 'backups',
	requires: actions.backupsList
};
const failedSetups: NextAction = {
	label: 'Open saved setups',
	section: 'setups',
	requires: actions.setupsManage
};
const failedUpdate: NextAction = {
	label: 'See the update notice',
	section: 'update',
	requires: actions.gameUpdate
};

const nextActions: Record<string, NextAction> = {
	start: failedStart,
	restart: failedStart,
	mod_install: failedMods,
	mod_uninstall: failedMods,
	mod_toggle: failedMods,
	backup: failedBackups,
	restore: failedBackups,
	setup_save: failedSetups,
	setup_restore: failedSetups,
	setup_delete: failedSetups,
	game_update: failedUpdate
};

/** Where to go after a failure, or null when the kind has no screen that helps. */
export function nextAction(job: Pick<Job, 'kind' | 'status'>): NextAction | null {
	if (job.status !== 'failed') return null;
	return Object.hasOwn(nextActions, job.kind) ? nextActions[job.kind] : null;
}
