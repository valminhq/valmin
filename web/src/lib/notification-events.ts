import type { GameKind, RuleKind } from '$lib/api/admin';
import type { InboxKind } from '$lib/api/inbox';
import { CONDITION_SENTENCE } from '$lib/conditions';
import type { NotifyEvent } from '$lib/message';

/** An event the Notifications page sets up: a condition the panel watches, or a server's start,
 * stop or a power cut. Samples are made up for the preview. */
export type PanelEvent = NotifyEvent<
	Exclude<RuleKind, GameKind>,
	'Server status' | 'Problems' | 'Maintenance'
>;
export type PanelKind = PanelEvent['kind'];

const ALERT = '#D83C3E';
const SERVER_TIME = '17 Oct 2026, 15:12 UTC';

/** A condition's event, with the daemon's headline and default message (delivery.summarize,
 * delivery.defaultTemplates). */
function condition(
	kind: InboxKind,
	group: PanelEvent['group'],
	label: string,
	headline: string,
	fields: [string, string, string][],
	whatToDo: string
): PanelEvent {
	const lines = [
		...fields.map(([key, name]) => `**${name}:** {${key}}`),
		'**What to do:** {what_to_do}'
	];
	return {
		kind,
		group,
		label,
		when: `${CONDITION_SENTENCE[kind]}.`,
		headline,
		accent: ALERT,
		template: lines.join('\n'),
		placeholders: [...fields.map(([key]) => key), 'what_to_do', 'server', 'event'],
		sample: {
			...Object.fromEntries(fields.map(([key, , value]) => [key, value])),
			what_to_do: whatToDo
		}
	};
}

export const PANEL_EVENTS: PanelEvent[] = [
	{
		kind: 'server_started',
		group: 'Server status',
		label: 'Server started',
		when: 'Valmin started or restarted a server and its world has loaded. On crossplay, once it has a join code.',
		headline: 'Server started',
		accent: '#2ECC71',
		template:
			'**Server name:** {server_name}\n**World:** {world}\n**Join code:** {join_code}\n**Port:** {port}\n**Started:** {time}',
		placeholders: ['server_name', 'world', 'join_code', 'port', 'time', 'server', 'event'],
		sample: {
			server_name: 'Midgard PvE',
			world: 'Dedicated',
			join_code: '482913',
			port: '2456',
			time: SERVER_TIME
		}
	},
	{
		kind: 'server_stopped',
		group: 'Server status',
		label: 'Server stopped',
		when: 'Valmin stopped a server: by hand, by auto-stop or for a power cut.',
		headline: 'Server stopped',
		accent: '#95A5A6',
		template:
			'**Reason:** {reason}\n**Server name:** {server_name}\n**World:** {world}\n**Stopped:** {time}',
		placeholders: ['reason', 'server_name', 'world', 'time', 'server', 'event'],
		sample: {
			reason: 'Stopped by alex',
			server_name: 'Midgard PvE',
			world: 'Dedicated',
			time: SERVER_TIME
		}
	},
	{
		kind: 'power_cut',
		group: 'Server status',
		label: 'Power cut soon',
		when: 'A planned power cut is seven minutes away and running servers are about to stop.',
		headline: 'Power cut soon: servers are stopping',
		accent: '#F1C40F',
		template:
			'**Power goes off:** {power_off}\n**Servers stop:** {stop_at}\n**Running servers:** {servers}',
		placeholders: ['power_off', 'stop_at', 'servers', 'event'],
		sample: {
			power_off: '17 Oct 2026, 23:00 UTC',
			stop_at: '17 Oct 2026, 22:55 UTC',
			servers: 'Midgard, Ashlands'
		}
	},
	condition(
		'crash_loop',
		'Problems',
		'Crash loop',
		'The server keeps crashing',
		[['stops', 'Unexpected stops', '3 in the last 30 minutes']],
		'Check the server’s console log for the error that stops it.'
	),
	condition(
		'instance_error',
		'Problems',
		'Server error',
		'The server needs a check after a failure',
		[],
		'Open the server in Valmin and choose Check this server. Its last failed job has the cause.'
	),
	condition(
		'unclean_stop',
		'Problems',
		'Unclean stop',
		'The server stopped before its world finished saving',
		[['job', 'Job', 'Stop']],
		'Recent progress may be lost. Check the world before players rejoin, and restore a backup if it is damaged.'
	),
	condition(
		'job_failed',
		'Problems',
		'Job failed',
		'Backup failed',
		[['reason', 'Reason', 'The archive could not be written.']],
		'Open the server’s job history for the full log, then try the job again.'
	),
	condition(
		'job_stuck',
		'Problems',
		'Job stuck',
		'Backup is taking unusually long',
		[['running', 'Running for', '1 hour 12 minutes']],
		'Check the job’s progress in Valmin, and cancel it if it is not moving.'
	),
	condition(
		'update_available',
		'Maintenance',
		'Update available',
		'A game update is available',
		[
			['installed', 'Installed build', '19841237'],
			['available', 'Available build', '20012345']
		],
		'Stop the server, then update it from its page in Valmin.'
	),
	condition(
		'restart_required',
		'Maintenance',
		'Restart required',
		'The server needs a restart to apply a change',
		[],
		'Restart the server when no one is playing.'
	),
	condition(
		'stale_backup',
		'Maintenance',
		'Backups stale',
		'Scheduled backups have stopped',
		[
			['schedule', 'Schedule', 'Every 6 hours'],
			['last_backup', 'Last backup', '17 Oct 2026, 03:00 UTC']
		],
		'Check the server’s recent backup jobs for failures.'
	),
	condition(
		'low_disk',
		'Maintenance',
		'Low disk',
		'The host is running low on disk space',
		[
			['free', 'Free space', '4.5 GB'],
			['alarm', 'Alert below', '10 GB']
		],
		'Delete old backups, worlds or saved setups, or free space on the host. Backups and updates fail once the disk is full.'
	)
];

export const PANEL_GROUPS: PanelEvent['group'][] = ['Server status', 'Problems', 'Maintenance'];

/** Kinds that never concern one server, so their rules cover every server. */
export const HOST_WIDE: PanelKind[] = ['low_disk', 'power_cut'];
/** Kinds whose messages show a time, so a rule can choose the zone. */
export const TIMED: PanelKind[] = ['power_cut', 'server_started', 'server_stopped', 'stale_backup'];
/** Whether a kind is a condition, sent when it opens and edited when it clears, rather than a
 * one-off event. */
export const isCondition = (kind: PanelKind) =>
	!['server_started', 'server_stopped', 'power_cut'].includes(kind);

/** alerts.Params.Defaults: what an empty threshold means. */
export const DEFAULT_CRASH_COUNT = 3;
export const DEFAULT_CRASH_WINDOW_MINUTES = 30;
export const DEFAULT_STUCK_MINUTES = 60;
export const DEFAULT_STALE_FACTOR = 2;

/** A template without the lines that show any of hidden, for a rule that hid fields before
 * templates could. */
export function withoutFields(template: string, hidden: string[]): string {
	return template
		.split('\n')
		.filter((line) => !hidden.some((key) => line.includes(`{${key}}`)))
		.join('\n');
}
