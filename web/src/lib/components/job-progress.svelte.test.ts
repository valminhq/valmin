import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeDaemon, job } from '$lib/testing/daemon';
import { socket } from '$lib/testing/socket';
import JobProgress from './job-progress.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	socket.reset();
	vi.unstubAllGlobals();
});

const done = {
	type: 'job' as const,
	id: 'job-1',
	kind: 'backup',
	status: 'succeeded',
	progress: 100,
	message: 'Done'
};

// A job can be reported finished twice: the row read and the socket event can both carry the
// terminal status. Whatever the screen does on completion — re-read, navigate, submit the next
// step — must happen once.
describe('job progress', () => {
	it('reports completion once, however many times the job is seen finished', async () => {
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ status: 'succeeded' })));
		const onfinish = vi.fn();
		render(JobProgress, { jobId: 'job-1', onfinish });

		await vi.waitFor(() => expect(onfinish).toHaveBeenCalledTimes(1));
		socket.push('job.job-1', done);
		socket.push('job.job-1', { ...done, message: 'Done again' });
		await screen.findByText(/Done again/);
		expect(onfinish).toHaveBeenCalledTimes(1);
	});

	it('renders the job the daemon reports, error included', async () => {
		daemon.on('GET', '/jobs/job-1', () =>
			Response.json(job({ status: 'failed', message: 'Stopped', error: 'Disk full.' }))
		);
		render(JobProgress, { jobId: 'job-1' });

		expect(await screen.findByText('Disk full.')).toBeTruthy();
	});
});
