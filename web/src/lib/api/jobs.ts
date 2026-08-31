import { api } from './client';
import type { Job } from './types';
import { topics } from '$lib/socket/messages';
import type { Socket } from '$lib/socket/client';
import type { JobMessage, ServerMessage } from '$lib/socket/messages';

const TERMINAL = new Set(['succeeded', 'failed', 'cancelled']);

export function isTerminal(status: string): boolean {
	return TERMINAL.has(status);
}

/**
 * Reconciles one live event or fetched row against what is already known.
 *
 * `↯` **Terminal status always wins over a late progress event** (G3, `12 §7`). The row and
 * the socket are two views of the same job arriving in an order nobody controls, and the
 * only ordering rule that holds is that a job does not un-finish. Without it a `202` that
 * completes in 300 ms renders as "60%, downloading…" forever, because the fetch answered
 * after the last live message.
 */
export function merge(current: Job | null, incoming: Job): Job {
	if (current === null) return incoming;
	if (isTerminal(current.status) && !isTerminal(incoming.status)) return current;
	return incoming;
}

function fromMessage(current: Job | null, message: JobMessage): Job {
	return {
		id: message.id,
		kind: message.kind || (current?.kind ?? ''),
		status: message.status as Job['status'],
		progress: message.progress,
		message: message.message,
		error_code: current?.error_code ?? null,
		error: current?.error ?? null,
		instance_id: current?.instance_id ?? null
	};
}

/**
 * Follows a job to its outcome: **subscribe, then fetch** (G3, `14 §7.2`).
 *
 * `↯` That order is the whole point, and it is written once here rather than in each
 * component (`06 §4`). Fetching first leaves a window in which the job finishes before the
 * subscription exists, and the client waits for an event that has already been sent. A
 * *terminal* job is still subscribable precisely so this ordering is implementable
 * (`14 §2.2`).
 */
export function watchJob(socket: Socket, jobId: string, onUpdate: (job: Job) => void): () => void {
	let current: Job | null = null;

	const apply = (incoming: Job) => {
		const next = merge(current, incoming);
		if (next === current) return;
		current = next;
		onUpdate(next);
	};

	const unsubscribe = socket.subscribe(topics.job(jobId), (message: ServerMessage) => {
		if (message.type !== 'job') return;
		apply(fromMessage(current, message));
	});

	api
		.get<Job>(`/jobs/${jobId}`)
		.then(apply)
		.catch(() => {
			// The socket is the live channel and the row is the checkpoint (`12 §7`). A read
			// that fails leaves the live channel doing its job; a component that needs the
			// row can ask again.
		});

	return unsubscribe;
}
