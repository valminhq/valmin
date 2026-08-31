import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ServerMessage } from '$lib/socket/messages';

/** The one socket, replaced by something that hands back the handler it was given. */
let delivered: ((m: ServerMessage) => void) | null = null;
let subscriptions = 0;

vi.mock('$lib/socket/index.svelte', () => ({
	socket: {
		subscribe(_topic: string, handler: (m: ServerMessage) => void) {
			delivered = handler;
			subscriptions += 1;
			return () => {
				delivered = null;
			};
		}
	},
	socketStatus: { value: 'open' }
}));

type LogLine = { ts: string; stream: 'stdout' | 'stderr'; line: string };
const logs = vi.fn<(id: string, tail?: number) => Promise<LogLine[]>>(async () => []);
vi.mock('$lib/api/instances', () => ({ instances: { logs } }));

const { ConsoleBuffer } = await import('./console.svelte');

function line(seq: number, text: string): ServerMessage {
	return {
		type: 'console',
		instance: 'i1',
		seq,
		ts: '2026-08-31T10:00:00Z',
		stream: 'stdout',
		line: text
	};
}

/**
 * `↯` Updates are published on a frame boundary, not per message — one array copy per frame
 * instead of one per line, which is what took 20 000 lines from 293 ms to 3.4 ms. So every
 * assertion about `rows` has to let a frame pass first, and `painted()` is that wait.
 */
const painted = () => new Promise((r) => setTimeout(r, 0));

function open() {
	const buffer = new ConsoleBuffer('i1');
	const off = buffer.open();
	return { buffer, off, send: (m: ServerMessage) => delivered?.(m) };
}

beforeEach(() => {
	delivered = null;
	subscriptions = 0;
	logs.mockClear();
});

describe('ConsoleBuffer', () => {
	it('renders contiguous lines with no break between them', async () => {
		const { buffer, send } = open();
		send({ type: 'subscribed', topic: 'instance.i1.console', seq: 3 });
		send(line(1, 'one'));
		send(line(2, 'two'));
		send(line(3, 'three'));
		await painted();
		expect(buffer.rows.map((r) => r.kind)).toEqual(['line', 'line', 'line']);
	});

	// `↯` `14 §4.2`. The replay is the pinned startup segment followed by the ring, and once
	// the ring has rotated past the segment those two are not adjacent — they only arrive
	// that way. Rendering them touching would let a reader conclude the boot led straight
	// into whatever is on the next line.
	it('a jump in seq across the replay becomes a visible break', async () => {
		const { buffer, send } = open();
		send({ type: 'subscribed', topic: 'instance.i1.console', seq: 3000 });
		send(line(1, 'container start'));
		send(line(2, 'chainloader'));
		send(line(3000, 'much later'));
		await painted();

		const kinds = buffer.rows.map((r) => r.kind);
		expect(kinds).toEqual(['line', 'line', 'break', 'line']);
		const gap = buffer.rows[2];
		expect(gap.kind === 'break' && gap.text).toContain('2997 lines are not shown');
	});

	// ADR-039: the hub dropped messages this consumer was too slow to take. Different cause,
	// same obligation — say so rather than closing the hole.
	it('a gap message becomes a visible break carrying the count', async () => {
		const { buffer, send } = open();
		send(line(1, 'one'));
		send({ type: 'gap', topic: 'instance.i1.console', dropped: 12, from_seq: 14 });
		send(line(14, 'later'));
		await painted();

		const gap = buffer.rows[1];
		expect(gap.kind === 'break' && gap.text).toContain('12 lines dropped');
		// `↯` And no *second* break from the seq jump the drop just created: the hub already
		// reported it, and saying it twice reads as two separate losses.
		expect(buffer.rows.map((r) => r.kind)).toEqual(['line', 'break', 'line']);
	});

	// `14 §4.2`: the reader restarted. Clear the view; do not splice. And ask again — a fresh
	// subscribe is what re-delivers the pinned startup segment, which is otherwise gone from
	// this view exactly when a daemon restart makes it worth reading (G8).
	it('stream.reset clears the view and re-subscribes', async () => {
		const { buffer, send } = open();
		send(line(1, 'one'));
		send(line(2, 'two'));
		await painted();
		expect(buffer.rows).toHaveLength(2);

		// `↯` A reset publishes immediately rather than waiting for a frame: the view is
		// stale by definition and showing it for another 16 ms is showing a lie.
		send({ type: 'stream.reset', topic: 'instance.i1.console' });
		expect(buffer.rows).toHaveLength(0);

		await new Promise((r) => queueMicrotask(() => r(null)));
		expect(subscriptions, 'the reset must re-subscribe, not just clear').toBe(2);
	});

	// `↯` G8. The startup segment is what explains a boot that failed, so a console that has
	// out-scrolled its own cap must drop the middle, never the head.
	it('trimming never eats the pinned startup segment', async () => {
		const { buffer, send } = open();
		send({ type: 'subscribed', topic: 'instance.i1.console', seq: 10 });
		for (let seq = 1; seq <= 10; seq += 1) send(line(seq, `boot ${seq}`));
		// Well past MAX_ROWS, all of it live.
		for (let seq = 11; seq <= 6000; seq += 1) send(line(seq, `live ${seq}`));
		await painted();

		const first = buffer.rows[0];
		expect(first.kind === 'line' && first.text).toBe('boot 1');
		const tenth = buffer.rows[9];
		expect(tenth.kind === 'line' && tenth.text).toBe('boot 10');
		// The head survived; the middle is what went.
		const eleventh = buffer.rows[10];
		expect(eleventh.kind === 'line' && eleventh.text).not.toBe('live 11');
	});

	// `14 §8` empties the ring on a daemon restart, so an instance that died last night has
	// no in-memory source at all. The acknowledgement carries seq 0 when there was no replay,
	// which is the one reliable signal that Docker's own log is the only source left.
	it('an empty replay falls back to the recorded log', async () => {
		logs.mockResolvedValueOnce([
			{ ts: '2026-08-31T09:00:00Z', stream: 'stdout', line: 'why it died' }
		]);
		const { buffer, send } = open();
		send({ type: 'subscribed', topic: 'instance.i1.console', seq: 0 });
		await vi.waitFor(() => expect(buffer.rows.length).toBeGreaterThan(0));

		expect(logs).toHaveBeenCalledWith('i1');
		const notice = buffer.rows[0];
		expect(notice.kind === 'break' && notice.text).toContain('recorded log');
		const recorded = buffer.rows[1];
		// `↯` seq 0: these came from Docker and carry none of the panel's sequence numbers.
		expect(recorded.kind === 'line' && recorded.seq).toBe(0);
	});

	// `↯` The perf invariant, stated as behaviour rather than as a stopwatch. Appending with
	// `[...rows, row]` cost 6.5 µs/line at 1 000 rows and 14.6 µs at 20 000 — the per-line
	// cost *grew with the buffer*, because every line copied the whole thing, and a verbose
	// BepInEx boot arrives in exactly the bursts that turns into a stall. Coalescing onto a
	// frame boundary took 20 000 lines from 293 ms to 3.4 ms. A timing assertion would be
	// flaky; this one is not, and it fails the moment anything publishes per message again.
	it('a burst of lines publishes once, not once per line', async () => {
		const { buffer, send } = open();
		const before = buffer.rows;
		for (let seq = 1; seq <= 1000; seq += 1) send(line(seq, `line ${seq}`));

		expect(buffer.rows, 'nothing may be published before the frame boundary').toBe(before);
		await painted();
		expect(buffer.rows).toHaveLength(1000);
	});

	it('a refused topic is reported rather than left as an empty console', () => {
		const { buffer, send } = open();
		send({
			type: 'error',
			topic: 'instance.i1.console',
			code: 'not_found',
			message: 'Not found.'
		});
		expect(buffer.error).toBe('not_found');
	});
});
