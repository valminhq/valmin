// The wire protocol of `04 §4`, as a discriminated union. The hub is a transport
// (ADR-042) and so is this: nothing here interprets a console line or a job payload.

export interface SubscribedMessage {
	type: 'subscribed';
	topic: string;
	seq: number;
}

export interface ConsoleMessage {
	type: 'console';
	instance: string;
	seq: number;
	ts: string;
	stream: 'stdout' | 'stderr';
	line: string;
}

export interface StatsMessage {
	type: 'stats';
	instance: string;
	ts: string;
	/** null on the first sample of a container, and for an idle *system* clock — never
	 * rendered as 0, or a dashboard opens at 0% for a server pegged at 300% (E10). */
	cpu_pct: number | null;
	mem_bytes: number;
	mem_limit: number;
	mem_pct: number | null;
	/** Always null on this build (E7, Q7). Render "unknown", never 0. */
	players: number | null;
}

export interface StateMessage {
	type: 'state';
	instance: string;
	state: string;
	restart_required: boolean;
}

export interface JobMessage {
	type: 'job';
	id: string;
	kind: string;
	status: string;
	progress: number;
	message: string;
	log?: string;
}

/** A visible break, not a seamless lie (ADR-039). */
export interface GapMessage {
	type: 'gap';
	topic: string;
	dropped: number;
	from_seq: number;
}

/** The log reader restarted: clear the view, do not splice (`14 §4.2`). */
export interface StreamResetMessage {
	type: 'stream.reset';
	topic: string;
}

export interface PongMessage {
	type: 'pong';
}

/** Per-topic failure, code from `11 §2.5`'s registry. */
export interface ErrorMessage {
	type: 'error';
	topic?: string;
	code: string;
	message: string;
}

export type ServerMessage =
	| SubscribedMessage
	| ConsoleMessage
	| StatsMessage
	| StateMessage
	| JobMessage
	| GapMessage
	| StreamResetMessage
	| PongMessage
	| ErrorMessage;

/**
 * The topic a message belongs to, or "" for one that belongs to the connection itself.
 *
 * Three of the message types carry their subject rather than their topic — a console
 * line names its instance, a job event its id — so the mapping lives here, once, next to
 * the types. A component that reassembled the topic string itself would be a second place
 * to get `instance.{id}.console` wrong.
 */
export function topicOf(message: ServerMessage): string {
	switch (message.type) {
		case 'console':
			return `instance.${message.instance}.console`;
		case 'stats':
			return `instance.${message.instance}.stats`;
		case 'state':
			return `instance.${message.instance}.state`;
		case 'job':
			return `job.${message.id}`;
		case 'subscribed':
		case 'gap':
		case 'stream.reset':
			return message.topic;
		case 'error':
			return message.topic ?? '';
		default:
			return '';
	}
}

export const topics = {
	console: (instanceId: string) => `instance.${instanceId}.console`,
	stats: (instanceId: string) => `instance.${instanceId}.stats`,
	state: (instanceId: string) => `instance.${instanceId}.state`,
	job: (jobId: string) => `job.${jobId}`
};
