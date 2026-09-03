import { instances as api, type Instance } from '$lib/api/instances';
import { socket } from '$lib/socket/index.svelte';
import { topics, type ServerMessage } from '$lib/socket/messages';

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

	async load(): Promise<void> {
		this.loading = true;
		try {
			const items = await api.list();
			this.items = items;
			this.error = null;
			this.watch();
		} catch (err) {
			this.error = err;
		} finally {
			this.loading = false;
		}
	}

	/** Drops every subscription — for a sign-out, where the topics are no longer ours. */
	release(): void {
		for (const off of this.subscriptions.values()) off();
		this.subscriptions.clear();
		this.items = [];
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
		if (message.type !== 'state') return;
		this.items = this.items.map((i) =>
			i.id === message.instance
				? { ...i, state: message.state, restart_required: message.restart_required }
				: i
		);
	}
}

export const instanceList = new InstanceList();
