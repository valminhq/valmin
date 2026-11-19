import { describe, expect, it, vi } from 'vitest';
import type { StatsReading } from '#lib/api/instances.js';
import type { ServerMessage } from '#lib/socket/messages.js';

let delivered: ((m: ServerMessage) => void) | null = null;

vi.mock('#lib/socket/index.svelte.js', () => ({
	socket: {
		subscribe(_topic: string, handler: (m: ServerMessage) => void) {
			delivered = handler;
			return () => {
				delivered = null;
			};
		}
	},
	socketStatus: { value: 'open' }
}));

let answer: (reading: StatsReading) => void = () => {};
const stats = vi.fn(() => new Promise<StatsReading>((resolve) => (answer = resolve)));
vi.mock('#lib/api/instances.js', () => ({ instances: { stats } }));

const { StatsWindow } = await import('./stats.svelte');

const reading = {
	cpu_pct: 10,
	mem_bytes: 1024,
	mem_limit: 4096,
	mem_pct: 25,
	players: null,
	rx_bytes_per_second: null,
	tx_bytes_per_second: null
};

describe('StatsWindow', () => {
	it('drops a seed that answers after a live sample', async () => {
		const window = new StatsWindow('i1');
		const close = window.open();

		delivered?.({ type: 'stats', instance: 'i1', ts: '2026-11-19T12:00:30Z', ...reading });
		answer({ available: true, ts: '2026-11-19T12:00:00Z', ...reading });
		await Promise.resolve();
		await Promise.resolve();

		expect(window.samples.map((s) => s.t)).toEqual([Date.parse('2026-11-19T12:00:30Z')]);
		expect(window.latest?.ts).toBe('2026-11-19T12:00:30Z');
		close();
	});
});
