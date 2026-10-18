import type { ServerMessage } from '$lib/socket/messages';

type Handler = (message: ServerMessage) => void;

/**
 * Stands in for `$lib/socket/index.svelte` in a screen test: nothing connects, and a test
 * pushes the messages the daemon would publish. Mock the module with
 * `vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'))`.
 */
class FakeSocket {
	private readonly handlers = new Map<string, Set<Handler>>();
	private readonly connected = new Set<() => void>();

	connect(): void {}
	close(): void {}

	subscribe(topic: string, handler: Handler): () => void {
		const set = this.handlers.get(topic) ?? new Set();
		set.add(handler);
		this.handlers.set(topic, set);
		return () => set.delete(handler);
	}

	onConnected(fn: () => void): () => void {
		this.connected.add(fn);
		return () => this.connected.delete(fn);
	}

	/** Fires every onConnected hook, as a reconnect would. */
	reconnect(): void {
		for (const fn of [...this.connected]) fn();
	}

	/** Delivers message to everything subscribed to topic, as a publish from the daemon would. */
	push(topic: string, message: ServerMessage): void {
		for (const handler of this.handlers.get(topic) ?? []) handler(message);
	}

	reset(): void {
		this.handlers.clear();
		this.connected.clear();
	}
}

export const socket = new FakeSocket();
export const socketStatus: { value: string } = { value: 'open' };
