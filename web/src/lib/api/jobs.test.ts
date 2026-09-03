import { describe, expect, it, vi } from 'vitest';
import { isTerminal, merge, watchJob } from './jobs';
import type { Job } from './types';
import { Socket } from '$lib/socket/client';
import type { ServerMessage } from '$lib/socket/messages';

function job(status: Job['status'], progress = 0): Job {
	return {
		job_id: 'job-1',
		kind: 'provision',
		status,
		progress,
		message: '',
		instance_id: 'inst-a',
		created_at: '2026-08-31T00:00:00Z'
	};
}

describe('merge (G3)', () => {
	// The row and the socket are two views of the same job arriving in an order nobody
	// controls, and the only ordering rule that holds is that a job does not un-finish.
	it('lets a terminal status win over a late progress event', () => {
		expect(merge(job('succeeded'), job('running', 60)).status).toBe('succeeded');
		expect(merge(job('failed'), job('running', 60)).status).toBe('failed');
		expect(merge(job('cancelled'), job('running', 60)).status).toBe('cancelled');
	});

	it('takes the newer value while the job is still running', () => {
		expect(merge(job('running', 10), job('running', 60)).progress).toBe(60);
		expect(merge(null, job('running', 10)).progress).toBe(10);
		expect(merge(job('running', 60), job('succeeded', 100)).status).toBe('succeeded');
	});

	it('knows which statuses are terminal', () => {
		expect(['succeeded', 'failed', 'cancelled'].every(isTerminal)).toBe(true);
		expect(['queued', 'running'].some(isTerminal)).toBe(false);
	});
});

/** A socket whose subscribe is a plain callback registry — the transport is not under test. */
function fakeSocket() {
	const handlers = new Map<string, (m: ServerMessage) => void>();
	const socket = {
		subscribe(topic: string, handler: (m: ServerMessage) => void) {
			handlers.set(topic, handler);
			return () => handlers.delete(topic);
		}
	} as unknown as Socket;
	return { socket, handlers };
}

describe('watchJob: subscribe, then fetch (G3, `14 §7.2`)', () => {
	// The ordering is written once in the client rather than in each component because a
	// `202` whose job finishes in 300 ms is the
	// normal case for `start`, and fetching first leaves a window where the client waits
	// forever for an event that was sent before it was listening.
	it('renders a job that finished before the subscription as complete', async () => {
		const { socket } = fakeSocket();
		const fetched = job('succeeded', 100);
		vi.spyOn(globalThis, 'fetch').mockResolvedValue(
			new Response(JSON.stringify(fetched), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
		);

		const seen: Job[] = [];
		watchJob(socket, 'job-1', (j) => seen.push(j));
		await vi.waitFor(() => expect(seen).toHaveLength(1));

		expect(seen[0].status).toBe('succeeded');
		vi.restoreAllMocks();
	});

	it('ignores a progress event that arrives after the job has finished', async () => {
		const { socket, handlers } = fakeSocket();
		vi.spyOn(globalThis, 'fetch').mockResolvedValue(
			new Response(JSON.stringify(job('succeeded', 100)), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
		);

		const seen: Job[] = [];
		watchJob(socket, 'job-1', (j) => seen.push(j));
		await vi.waitFor(() => expect(seen).toHaveLength(1));

		handlers.get('job.job-1')?.({
			type: 'job',
			id: 'job-1',
			kind: 'provision',
			status: 'running',
			progress: 60,
			message: 'Downloading'
		});

		expect(seen.at(-1)?.status).toBe('succeeded');
		vi.restoreAllMocks();
	});

	it('follows live progress before the fetch settles', async () => {
		const { socket, handlers } = fakeSocket();
		vi.spyOn(globalThis, 'fetch').mockImplementation(
			() =>
				new Promise((resolve) =>
					setTimeout(
						() =>
							resolve(
								new Response(JSON.stringify(job('running', 10)), {
									status: 200,
									headers: { 'Content-Type': 'application/json' }
								})
							),
						5
					)
				)
		);

		const seen: Job[] = [];
		watchJob(socket, 'job-1', (j) => seen.push(j));
		handlers.get('job.job-1')?.({
			type: 'job',
			id: 'job-1',
			kind: 'provision',
			status: 'running',
			progress: 60,
			message: 'Downloading'
		});
		expect(seen.at(-1)?.progress).toBe(60);

		await vi.waitFor(() => expect(seen).toHaveLength(2));
		vi.restoreAllMocks();
	});
});
