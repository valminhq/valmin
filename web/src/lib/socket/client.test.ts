import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CLOSE, Socket } from './client';
import type { ServerMessage } from './messages';

/** The three handlers and two methods the client actually uses. */
class FakeSocket {
	static instances: FakeSocket[] = [];

	readyState = 0;
	sent: string[] = [];
	onopen: (() => void) | null = null;
	onmessage: ((event: MessageEvent) => void) | null = null;
	onclose: ((event: CloseEvent) => void) | null = null;
	onerror: (() => void) | null = null;

	constructor(readonly url: string) {
		FakeSocket.instances.push(this);
	}

	send(data: string) {
		this.sent.push(data);
	}

	close(code = CLOSE.normal) {
		this.fire(code);
	}

	/** Completes the handshake. */
	accept() {
		this.readyState = 1;
		this.onopen?.();
	}

	/** Drops the connection with a close code, the way the hub does (`14 §3.4`). */
	fire(code: number) {
		this.readyState = 3;
		this.onclose?.({ code } as CloseEvent);
	}

	deliver(message: ServerMessage) {
		this.onmessage?.({ data: JSON.stringify(message) } as MessageEvent);
	}

	get subscriptions(): string[] {
		return this.sent
			.map((raw) => JSON.parse(raw) as { type: string; topics?: string[] })
			.filter((m) => m.type === 'subscribe')
			.flatMap((m) => m.topics ?? []);
	}
}

function connect(hooks = {}) {
	const socket = new Socket(hooks, 'ws://panel.test/api/v1/ws', (url) => {
		return new FakeSocket(url) as unknown as WebSocket;
	});
	socket.connect();
	return socket;
}

const latest = () => FakeSocket.instances[FakeSocket.instances.length - 1];

beforeEach(() => {
	FakeSocket.instances = [];
	vi.useFakeTimers();
	// Pin the jitter so a backoff assertion is about the ceiling, not about luck.
	vi.spyOn(Math, 'random').mockReturnValue(0);
});

afterEach(() => {
	vi.useRealTimers();
	vi.restoreAllMocks();
});

describe('reconnect', () => {
	// ADR-041: subscriptions are connection-scoped. There is no server-side subscription
	// state keyed to anything but the connection, so after any close the client re-subscribes
	// from scratch — and this is the acceptance criterion for killing the daemon and
	// restarting it without a page reload.
	it('re-subscribes from scratch on a new connection', () => {
		const socket = connect();
		socket.subscribe('instance.a.console', () => {});
		socket.subscribe('instance.a.state', () => {});
		latest().accept();
		expect(latest().subscriptions).toEqual(
			expect.arrayContaining(['instance.a.console', 'instance.a.state'])
		);

		// The daemon goes away mid-stream.
		latest().fire(CLOSE.internal);
		expect(socket.status).toBe('closed');

		vi.advanceTimersByTime(30_000);
		expect(FakeSocket.instances).toHaveLength(2);

		latest().accept();
		expect(socket.status).toBe('open');
		expect(latest().subscriptions.sort()).toEqual(['instance.a.console', 'instance.a.state']);
	});

	// `↯` The jitter is not decoration (`14 §7.1`): a panel restart closes every open tab at
	// the same instant, and without it they all come back at the same instant too. With
	// Math.random pinned to 0 the delay is exactly half the ceiling, so the series is
	// asserted rather than merely bounded.
	it('backs off exponentially and caps at 30 seconds', () => {
		connect();
		const expected = [250, 500, 1000, 2000, 4000, 8000, 15000, 15000, 15000];

		// Deliberately never accepted: a handshake that keeps failing is what escalates. A
		// connection that *opens* resets the series, which the next test pins.
		for (const delay of expected) {
			const before = FakeSocket.instances.length;
			latest().fire(CLOSE.internal);

			vi.advanceTimersByTime(delay - 1);
			expect(FakeSocket.instances, `reconnected before ${delay}ms`).toHaveLength(before);
			vi.advanceTimersByTime(1);
			expect(FakeSocket.instances, `did not reconnect at ${delay}ms`).toHaveLength(before + 1);
		}
	});

	it('forgets the backoff once a connection succeeds', () => {
		connect();
		for (let i = 0; i < 4; i++) {
			latest().accept();
			latest().fire(CLOSE.internal);
			vi.advanceTimersByTime(30_000);
		}
		// A connection that opened and then dropped starts the series again, or a server
		// that flaps once an hour ends the day retrying every thirty seconds.
		latest().accept();
		const before = FakeSocket.instances.length;
		latest().fire(CLOSE.internal);
		vi.advanceTimersByTime(249);
		expect(FakeSocket.instances).toHaveLength(before);
		vi.advanceTimersByTime(1);
		expect(FakeSocket.instances).toHaveLength(before + 1);
	});

	// `14 §7.1`: 1000, 1008 and 1009 mean do not come back. A client that retried a policy
	// violation would hammer the panel with a request it has already been told is wrong.
	it.each([
		['normal', CLOSE.normal],
		['policy violation', CLOSE.policy],
		['message too large', CLOSE.tooLarge]
	])('does not reconnect after %s', (_name, code) => {
		connect();
		latest().accept();
		latest().fire(code);
		vi.advanceTimersByTime(120_000);
		expect(FakeSocket.instances).toHaveLength(1);
	});
});

describe('revocation close codes (`14 §6`)', () => {
	// `↯` 4401 and 4403 are distinct on purpose. A client that conflates them signs the user
	// out because an admin narrowed one grant.
	it('4401 ends the session and stops reconnecting', () => {
		const onSessionExpired = vi.fn();
		const onAccessRevoked = vi.fn();
		connect({ onSessionExpired, onAccessRevoked });
		latest().accept();
		latest().fire(CLOSE.sessionExpired);

		expect(onSessionExpired).toHaveBeenCalledTimes(1);
		expect(onAccessRevoked).not.toHaveBeenCalled();
		vi.advanceTimersByTime(120_000);
		expect(FakeSocket.instances).toHaveLength(1);
	});

	it('4403 re-reads permissions and comes back', () => {
		const onSessionExpired = vi.fn();
		const onAccessRevoked = vi.fn();
		connect({ onSessionExpired, onAccessRevoked });
		latest().accept();
		latest().fire(CLOSE.accessRevoked);

		expect(onAccessRevoked).toHaveBeenCalledTimes(1);
		expect(onSessionExpired).not.toHaveBeenCalled();
		vi.advanceTimersByTime(60_000);
		expect(FakeSocket.instances.length).toBeGreaterThan(1);
	});
});

describe('delivery', () => {
	it('routes each message to its topic, including gap and stream.reset', () => {
		const console: ServerMessage[] = [];
		const state: ServerMessage[] = [];
		const socket = connect();
		socket.subscribe('instance.a.console', (m) => console.push(m));
		socket.subscribe('instance.b.state', (m) => state.push(m));
		latest().accept();

		latest().deliver({
			type: 'console',
			instance: 'a',
			seq: 1,
			ts: '2026-08-31T00:00:00Z',
			stream: 'stdout',
			line: 'hello'
		});
		latest().deliver({ type: 'gap', topic: 'instance.a.console', dropped: 12, from_seq: 2 });
		latest().deliver({ type: 'stream.reset', topic: 'instance.a.console' });
		latest().deliver({ type: 'state', instance: 'b', state: 'running', restart_required: false });
		// A topic nobody holds is dropped rather than fanned out to everyone.
		latest().deliver({ type: 'state', instance: 'zzz', state: 'stopped', restart_required: false });

		expect(console.map((m) => m.type)).toEqual(['console', 'gap', 'stream.reset']);
		expect(state.map((m) => m.type)).toEqual(['state']);
	});

	it('unsubscribing the last handler releases the topic', () => {
		const socket = connect();
		const off = socket.subscribe('instance.a.console', () => {});
		const stillWatching = socket.subscribe('instance.a.console', () => {});
		latest().accept();

		off();
		expect(socket.subscribed).toEqual(['instance.a.console']);
		stillWatching();
		expect(socket.subscribed).toEqual([]);
	});

	it('remembers topics subscribed while the socket is down', () => {
		const socket = new Socket({}, 'ws://panel.test/api/v1/ws', (url) => {
			return new FakeSocket(url) as unknown as WebSocket;
		});
		socket.subscribe('instance.a.console', () => {});
		socket.connect();
		latest().accept();
		expect(latest().subscriptions).toEqual(['instance.a.console']);
	});
});
