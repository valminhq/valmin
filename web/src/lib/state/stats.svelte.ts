import { instances as api, type StatsReading } from '#lib/api/instances.js';
import { socket } from '#lib/socket/index.svelte.js';
import { topics, type ServerMessage } from '#lib/socket/messages.js';

export interface Sample {
	t: number;
	cpu: number | null;
	mem: number | null;
	rx: number | null;
	tx: number | null;
}

/** Four minutes at the sampler's 2 s cadence (`14 §4.3`). A sparkline, not a history. */
const WINDOW = 120;

/**
 * One instance's resource readings, kept live.
 *
 * Subscribe, then fetch (G3, `14 §7.2`). The seed read matters here more than anywhere:
 * the socket's first sample carries `cpu_pct: null` because a percentage is a delta and it
 * has no predecessor (E10), so a page opened on a server that has been up for hours would
 * otherwise show "unknown" for two seconds. `GET /stats` serves the sampler's last sample,
 * which does have one.
 */
export class StatsWindow {
	samples = $state.raw<Sample[]>([]);
	latest = $state<StatsReading | null>(null);

	constructor(private instanceId: string) {}

	/** Subscribes and seeds. Returns the teardown, so an `$effect` can return it directly. */
	open(): () => void {
		const off = socket.subscribe(topics.stats(this.instanceId), (m: ServerMessage) =>
			this.apply(m)
		);
		void this.seed();
		return off;
	}

	private async seed(): Promise<void> {
		try {
			const reading = await api.stats(this.instanceId);
			// A live sample that arrived first wins: it is newer by construction.
			if (!this.latest) this.latest = reading;
			if (reading.available && reading.ts) this.record(reading.ts, reading);
		} catch {
			// A seed that fails is not an error to show: the socket is the live source and
			// this only fills the first two seconds.
		}
	}

	private apply(m: ServerMessage): void {
		if (m.type !== 'stats') return;
		this.latest = {
			available: true,
			ts: m.ts,
			cpu_pct: m.cpu_pct,
			mem_bytes: m.mem_bytes,
			mem_limit: m.mem_limit,
			mem_pct: m.mem_pct,
			players: m.players,
			rx_bytes_per_second: m.rx_bytes_per_second,
			tx_bytes_per_second: m.tx_bytes_per_second
		};
		this.record(m.ts, m);
	}

	private record(ts: string, r: Omit<StatsReading, 'available' | 'ts'>): void {
		const t = Date.parse(ts);
		if (Number.isNaN(t)) return;
		const sample = {
			t,
			cpu: r.cpu_pct,
			mem: r.mem_bytes,
			rx: r.rx_bytes_per_second,
			tx: r.tx_bytes_per_second
		};
		const next = [...this.samples, sample];
		this.samples = next.length > WINDOW ? next.slice(next.length - WINDOW) : next;
	}
}
