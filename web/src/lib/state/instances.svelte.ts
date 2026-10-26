import { instances as api, type Instance } from '#lib/api/instances.js';
import { socket } from '#lib/socket/index.svelte.js';
import { topics, type ServerMessage } from '#lib/socket/messages.js';
import { latestCall } from '#lib/latest.js';

/**
 * The instance list, kept live.
 *
 * Subscribe, then fetch (G3, `14 §7.2`), and re-fetch on every reconnect. A live stream
 * cannot say what it missed while the socket was down, so the list is re-read whenever one
 * comes back — anything else races, and the failure is a dashboard showing a running server
 * that stopped an hour ago.
 */
class InstanceList {
	items = $state<Instance[]>([]);
	loading = $state(true);
	error = $state<unknown>(null);

	private subscriptions = new Map<string, () => void>();
	private started = false;
	private begin = latestCall();

	/** Loads once if nothing has. A deep link to a server section never runs the dashboard's
	 * own load, so the shell would have no name for the heading or the document title. */
	async ensure(): Promise<void> {
		if (this.started) return;
		this.started = true;
		await this.load();
	}

	async load(): Promise<void> {
		this.started = true;
		this.loading = true;
		const current = this.begin();
		try {
			const items = await api.list();
			if (!current()) return;
			this.items = items;
			this.error = null;
			this.watch();
		} catch (err) {
			if (current()) this.error = err;
		} finally {
			if (current()) this.loading = false;
		}
	}

	/** Drops every subscription — for a sign-out, where the topics are no longer ours. */
	release(): void {
		for (const off of this.subscriptions.values()) off();
		this.subscriptions.clear();
		this.items = [];
		this.started = false;
	}

	/** One `instance.{id}.state` subscription per row, added and removed as the list changes
	 * (ADR-040: there is no wildcard, and a dashboard re-subscribes when its list does). */
	private watch(): void {
		const live = new Set(this.items.map((i) => i.id));

		for (const [id, off] of this.subscriptions) {
			if (!live.has(id)) {
				off();
				this.subscriptions.delete(id);
			}
		}
		for (const id of live) {
			if (this.subscriptions.has(id)) continue;
			this.subscriptions.set(
				id,
				socket.subscribe(topics.state(id), (message: ServerMessage) => this.apply(message))
			);
		}
	}

	private apply(message: ServerMessage): void {
		if (message.type === 'error') {
			// A revoked grant drops the topic; the server is no longer this caller's to show.
			this.items = this.items.filter((i) => topics.state(i.id) !== message.topic);
			this.watch();
			return;
		}
		this.items = this.items.map((i) => withLive(i, message));
	}
}

/** The row with what a state-topic message says about it, or the row itself when it says nothing. */
export function withLive(row: Instance, message: ServerMessage): Instance {
	if (message.type === 'join_code' && message.instance === row.id)
		return { ...row, crossplay_join_code: message.code };
	if (message.type === 'state' && message.instance === row.id)
		return {
			...row,
			state: message.state,
			restart_required: message.restart_required,
			pending_restart: message.pending_restart
		};
	return row;
}

export const instanceList = new InstanceList();
