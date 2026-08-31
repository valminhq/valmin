import { instances as api } from '$lib/api/instances';
import { socket } from '$lib/socket/index.svelte';
import { topics, type ServerMessage } from '$lib/socket/messages';

/** A rendered row: a console line, or a visible break where lines are missing. */
export type Row =
	| { kind: 'line'; seq: number; ts: string; stream: 'stdout' | 'stderr'; text: string }
	| { kind: 'break'; seq: number; text: string };

/**
 * How many rows the view keeps. The server's ring is 1000 lines (`14 §4.2`); this is larger
 * so a boot watched live is not thrown away while it is still on screen.
 */
const MAX_ROWS = 5000;

/**
 * The pinned prefix, matching the server's own `StartupLines`. `↯` G8: those rows are never
 * trimmed. They explain a failed boot and they are the first the server's ring drops.
 */
const PINNED_MAX = 500;

/**
 * One instance's console, kept live.
 *
 * `↯` Two kinds of missing lines, one rendering (ADR-039, `14 §4.2`). A `gap` is the hub
 * saying it dropped messages this browser was too slow to take; a jump in `seq` across the
 * replay is the server's ring having rotated past the pinned startup segment, so the boot
 * lines and the recent ones arrive adjacent without being adjacent. Neither is spliced
 * silently — a console that quietly closes a hole is worse than one that admits it, because
 * the reader draws conclusions from adjacency.
 */
export class ConsoleBuffer {
	rows = $state.raw<Row[]>([]);
	/** Set when the topic itself was refused — `not_found` for an instance this user cannot
	 * see (D2), so the page says so rather than showing an empty console forever. */
	error = $state<string | null>(null);

	/** Rows at the head that trimming may not touch: the replayed startup segment. */
	private pinned = 0;
	/** True while the rows arriving still belong to the pinned segment. */
	private pinning = true;
	/** The last sequence the replay carries, from the `subscribed` acknowledgement, which
	 * the hub sends *before* the replay it describes (`14 §2.3`). */
	private replayUntil = 0;
	private lastSeq = 0;
	private off: (() => void) | null = null;
	private handler = (m: ServerMessage) => this.apply(m);

	constructor(private instanceId: string) {}

	/** Subscribes. Returns the teardown, so an `$effect` can return it directly. */
	open(): () => void {
		this.off = socket.subscribe(topics.console(this.instanceId), this.handler);
		return () => {
			this.off?.();
			this.off = null;
		};
	}

	/** How many rows at the head are the pinned startup segment, for the jump control (G8). */
	get startupRows(): number {
		return this.pinned;
	}

	private apply(m: ServerMessage): void {
		switch (m.type) {
			case 'console':
				this.line(m.seq, m.ts, m.stream, m.line);
				break;
			case 'gap':
				this.push({
					kind: 'break',
					seq: m.from_seq,
					text: `${m.dropped} ${m.dropped === 1 ? 'line' : 'lines'} dropped — this browser fell behind the server`
				});
				this.pinning = false;
				this.lastSeq = 0;
				break;
			case 'stream.reset':
				// `↯` Clear, do not splice (`14 §4.2`), and then ask again. A reset means the
				// reader restarted; re-subscribing is what re-delivers the pinned startup
				// segment, which is otherwise lost from this view exactly when a daemon
				// restart makes it the thing worth reading.
				this.reset();
				this.resubscribe();
				break;
			case 'subscribed':
				this.replayUntil = m.seq;
				// `↯` A replay that carries nothing means there is no ring: the container is
				// stopped, or the daemon has restarted since it ran (`14 §8`). That is the
				// case this page exists for — "why did it die last night" — so fall back to
				// what Docker still holds. The acknowledgement arrives *before* the replay it
				// describes, so seq 0 is a reliable "there was none".
				if (m.seq === 0 && this.rows.length === 0) void this.loadRecorded();
				break;
			case 'error':
				this.error = m.code;
				break;
			default:
				break;
		}
	}

	private line(seq: number, ts: string, stream: 'stdout' | 'stderr', text: string): void {
		if (this.lastSeq && seq > this.lastSeq + 1) {
			const missing = seq - this.lastSeq - 1;
			this.push({
				kind: 'break',
				seq,
				text: `${missing} ${missing === 1 ? 'line is' : 'lines are'} not shown — the server's buffer rotated past them`
			});
			this.pinning = false;
		}
		this.lastSeq = Math.max(this.lastSeq, seq);
		this.push({ kind: 'line', seq, ts, stream, text });

		if (!this.pinning) return;
		if (seq > this.replayUntil) {
			// Past the replay: everything from here is live, and the segment is closed.
			this.pinning = false;
			return;
		}
		this.pinned = Math.min(this.rows.length, PINNED_MAX);
	}

	private push(row: Row): void {
		const next = [...this.rows, row];
		if (next.length > MAX_ROWS) {
			// Trim behind the pinned prefix, never through it.
			next.splice(this.pinned, next.length - MAX_ROWS);
		}
		this.rows = next;
	}

	/**
	 * Fills an empty console from `GET /logs`.
	 *
	 * `↯` These lines carry no `seq` and are never spliced into the live sequence: they come
	 * from Docker, and the panel's sequence numbers are the ring buffer's (`14 §4.2`). They
	 * are laid down under a break that says where they came from, so nothing about the view
	 * claims a continuity neither side promised.
	 */
	private async loadRecorded(): Promise<void> {
		let lines;
		try {
			lines = await api.logs(this.instanceId);
		} catch {
			return;
		}
		if (this.rows.length > 0 || lines.length === 0) return;
		this.rows = [
			{ kind: 'break', seq: 0, text: 'recorded log — the panel was not listening when this ran' },
			...lines.map((l): Row => ({ kind: 'line', seq: 0, ts: l.ts, stream: l.stream, text: l.line }))
		];
	}

	private reset(): void {
		this.rows = [];
		this.pinned = 0;
		this.pinning = true;
		this.replayUntil = 0;
		this.lastSeq = 0;
	}

	/** Off this tick: `off()` and `subscribe()` both mutate the set the socket is currently
	 * iterating in order to deliver this very message. */
	private resubscribe(): void {
		queueMicrotask(() => {
			if (!this.off) return;
			this.off();
			this.off = socket.subscribe(topics.console(this.instanceId), this.handler);
		});
	}
}
