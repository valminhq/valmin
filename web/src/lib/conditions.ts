import type { InboxItem, InboxKind } from '$lib/api/inbox';

/**
 * Short copy for a chip on a server card. The daemon sends facts; every word an operator
 * reads is written here.
 */
export const CONDITION_LABEL: Record<InboxKind, string> = {
	job_failed: 'job failed',
	low_disk: 'low disk',
	stale_backup: 'backups stale',
	unclean_stop: 'unclean stop',
	restart_required: 'restart required',
	update_available: 'update available',
	instance_error: 'error',
	crash_loop: 'crash loop',
	job_stuck: 'job stuck'
};

/** Full copy, for the host band and for a chip's tooltip. */
export const CONDITION_SENTENCE: Record<InboxKind, string> = {
	job_failed: 'A job failed and nothing has retried it',
	low_disk: 'The host is running out of disk space',
	stale_backup: 'Scheduled backups are not producing archives',
	unclean_stop: 'A server stopped before its world finished saving',
	restart_required: 'A change is waiting for a restart',
	update_available: 'A game update is available',
	instance_error: 'A server is in an error state',
	crash_loop: 'A server is crashing repeatedly',
	job_stuck: 'A job has been running unusually long'
};

/**
 * Kinds a server card already states through its own badges: StateBadge renders the error
 * state and the pending restart. A chip for either would put one fact on the card twice,
 * which is what moving conditions onto the card was meant to stop.
 */
export const ALREADY_ON_CARD: readonly InboxKind[] = ['restart_required', 'instance_error'];

/** Critical before warning, then longest-standing first. */
function compare(a: InboxItem, b: InboxItem): number {
	if (a.severity !== b.severity) return a.severity === 'critical' ? -1 : 1;
	return Date.parse(a.since_at) - Date.parse(b.since_at);
}

/** The conditions worth a chip on one server's card, in the order they should be read. */
export function chipsFor(items: InboxItem[], instanceId: string): InboxItem[] {
	return items
		.filter((item) => item.instance_id === instanceId && !ALREADY_ON_CARD.includes(item.kind))
		.sort(compare);
}

/** Host-wide conditions, plus any whose server is not on the list to carry it. */
export function bandItems(items: InboxItem[], listedIds: string[]): InboxItem[] {
	return items
		.filter((item) => !item.instance_id || !listedIds.includes(item.instance_id))
		.sort(compare);
}

export function formatBytes(value: string | undefined): string {
	const n = Number(value);
	if (!Number.isFinite(n)) return '';
	const units = ['B', 'KB', 'MB', 'GB', 'TB'];
	let size = n;
	let unit = 0;
	while (size >= 1024 && unit < units.length - 1) {
		size /= 1024;
		unit++;
	}
	return `${size < 10 ? size.toFixed(1) : Math.round(size)} ${units[unit]}`;
}

/**
 * The kind's own numbers, where they are what an operator acts on. Free space is spelled
 * out because it is why a world stops saving with no error anywhere.
 */
export function conditionDetail(item: InboxItem): string {
	if (item.kind === 'low_disk') {
		return `${formatBytes(item.detail?.Free)} free, below the ${formatBytes(item.detail?.Alarm)} floor`;
	}
	if (item.kind === 'job_failed' || item.kind === 'job_stuck') {
		return [item.detail?.Job, item.detail?.Error, item.detail?.Running].filter(Boolean).join(' · ');
	}
	if (item.kind === 'crash_loop') {
		return `${item.detail?.Stops} stops in ${item.detail?.Window}`;
	}
	if (item.kind === 'stale_backup') {
		return item.detail?.Last ? `last archive ${item.detail.Last}` : 'no archive on record';
	}
	return '';
}

/** How long the condition has been open, as a compact age. */
export function conditionAge(item: InboxItem, now: number = Date.now()): string {
	const minutes = Math.max(0, Math.round((now - Date.parse(item.since_at)) / 60000));
	if (minutes < 60) return `${minutes}m`;
	const hours = Math.round(minutes / 60);
	return hours < 48 ? `${hours}h` : `${Math.round(hours / 24)}d`;
}

/** The full text a chip carries as its tooltip: what it means, its numbers, and its age. */
export function conditionTitle(item: InboxItem): string {
	return [CONDITION_SENTENCE[item.kind], conditionDetail(item), `open ${conditionAge(item)}`]
		.filter(Boolean)
		.join(' · ');
}
