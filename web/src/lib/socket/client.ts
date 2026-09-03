import { csrfToken } from '$lib/api/client';
import { topicOf, type ServerMessage } from './messages';

/**
 * Close codes from `14 §3.4`. The two four-thousand codes are distinct on purpose: `4401`
 * means *you are no longer signed in*, `4403` means *you are, but not to this*. A client
 * that conflates them signs the user out because an admin narrowed one grant.
 */
export const CLOSE = {
	normal: 1000,
	goingAway: 1001,
	policy: 1008,
	tooLarge: 1009,
	internal: 1011,
	sessionExpired: 4401,
	accessRevoked: 4403
} as const;

/** Codes that mean "do not come back" (`14 §7.1`). Everything else is retried. */
const TERMINAL = new Set<number>([CLOSE.normal, CLOSE.policy, CLOSE.tooLarge]);

export type SocketStatus = 'closed' | 'connecting' | 'open';

export interface SocketHooks {
	/** `4401`: the session's absolute expiry fired. Redirect to login. */
	onSessionExpired?: () => void;
	/** `4403`: re-fetch permissions, then let the socket reconnect. */
	onAccessRevoked?: () => void;
	onStatus?: (status: SocketStatus) => void;
}

type Handler = (message: ServerMessage) => void;

const BACKOFF_MIN_MS = 500;
const BACKOFF_MAX_MS = 30_000;

/**
 * The panel's one WebSocket, multiplexed by topic (`04 §4`, `14`).
 *
 * Hand-rolled, written once, and this is where the reconnect contract lives (`06 §4`,
 * `14 §7`) — not in each component. Subscriptions are connection-scoped (ADR-041): there is
 * no server-side subscription state to resume, so after any close the client re-subscribes
 * from scratch, and it re-fetches over REST because a live stream cannot tell it what it
 * missed while it was gone.
 */
export class Socket {
	status: SocketStatus = 'closed';

	private url: string;
	private hooks: SocketHooks;
	private ws: WebSocket | null = null;
	private handlers = new Map<string, Set<Handler>>();
	private attempt = 0;
	private retry: ReturnType<typeof setTimeout> | null = null;
	private stopped = true;
	private open: (url: string) => WebSocket;

	constructor(hooks: SocketHooks = {}, url?: string, open?: (url: string) => WebSocket) {
		this.hooks = hooks;
		this.url = url ?? '';
		this.open = open ?? ((u) => new WebSocket(u));
	}

	/** Opens the connection, if it is not already opening or open. */
	connect(): void {
		this.stopped = false;
		if (this.ws || this.retry) return;
		this.dial();
	}

	/** Closes for good: no reconnect, no pending retry. */
	close(): void {
		this.stopped = true;
		if (this.retry) clearTimeout(this.retry);
		this.retry = null;
		this.ws?.close(CLOSE.normal, 'client closed');
		this.ws = null;
		this.setStatus('closed');
	}

	/**
	 * Starts receiving a topic's messages and returns the unsubscribe.
	 *
	 * Subscribing while the socket is down is legal and is the normal case during a
	 * reconnect: the topic is remembered and sent when the connection opens.
	 */
	subscribe(topic: string, handler: Handler): () => void {
		let handlers = this.handlers.get(topic);
		if (!handlers) {
			handlers = new Set();
			this.handlers.set(topic, handlers);
			this.send({ type: 'subscribe', topics: [topic] });
		}
		handlers.add(handler);

		return () => {
			const set = this.handlers.get(topic);
			if (!set) return;
			set.delete(handler);
			if (set.size > 0) return;
			this.handlers.delete(topic);
			this.send({ type: 'unsubscribe', topics: [topic] });
		};
	}

	/** The topics this client believes it holds. Exposed for tests and for a status line. */
	get subscribed(): string[] {
		return [...this.handlers.keys()];
	}

	private dial(): void {
		this.setStatus('connecting');
		const ws = this.open(this.endpoint());
		this.ws = ws;

		ws.onopen = () => {
			this.attempt = 0;
			this.setStatus('open');
			// From scratch, every time (ADR-041). The server keeps no subscription state
			// keyed to anything but the connection, and would have to re-authorize on resume
			// anyway — at which point the client may as well have asked.
			const topics = this.subscribed;
			if (topics.length > 0) this.send({ type: 'subscribe', topics });
		};

		ws.onmessage = (event: MessageEvent) => {
			let message: ServerMessage;
			try {
				message = JSON.parse(String(event.data)) as ServerMessage;
			} catch {
				return;
			}
			this.deliver(message);
		};

		ws.onclose = (event: CloseEvent) => {
			this.ws = null;
			this.setStatus('closed');
			if (event.code === CLOSE.sessionExpired) {
				this.stopped = true;
				this.hooks.onSessionExpired?.();
				return;
			}
			if (event.code === CLOSE.accessRevoked) {
				// The user is still signed in; what they may see changed. Re-read the
				// permissions before coming back, or the UI reconnects and re-subscribes to
				// a topic it has just been told it cannot have.
				this.hooks.onAccessRevoked?.();
			}
			if (this.stopped || TERMINAL.has(event.code)) {
				this.stopped = true;
				return;
			}
			this.scheduleRetry();
		};

		ws.onerror = () => {
			// A failed handshake reports an error and then a close; the close is where the
			// retry decision is made, so there is nothing to do here.
		};
	}

	private endpoint(): string {
		if (this.url) return this.url;
		const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
		// The token goes in the query string because a browser cannot set a header on the
		// WebSocket constructor (`11 §6.3`).
		return `${scheme}//${location.host}/api/v1/ws?csrf=${encodeURIComponent(csrfToken())}`;
	}

	/**
	 * Exponential backoff with jitter, capped (`14 §7.1`).
	 *
	 * The jitter is not decoration: a panel restart closes every open tab at the same
	 * instant, and without it they all come back at the same instant too.
	 */
	private scheduleRetry(): void {
		const ceiling = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** this.attempt);
		this.attempt += 1;
		const delay = ceiling / 2 + Math.random() * (ceiling / 2);
		this.retry = setTimeout(() => {
			this.retry = null;
			if (!this.stopped) this.dial();
		}, delay);
	}

	private deliver(message: ServerMessage): void {
		const topic = topicOf(message);
		if (!topic) return;
		for (const handler of this.handlers.get(topic) ?? []) handler(message);
	}

	private send(payload: unknown): void {
		if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(payload));
	}

	private setStatus(status: SocketStatus): void {
		if (this.status === status) return;
		this.status = status;
		this.hooks.onStatus?.(status);
	}
}
