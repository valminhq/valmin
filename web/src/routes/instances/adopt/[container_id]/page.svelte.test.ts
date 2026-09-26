import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { actions, type AdoptionPreview } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, gameOptions, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: {
		params: { container_id: 'c0ffee' },
		url: new URL('http://localhost/instances/adopt/c0ffee')
	}
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

const found: AdoptionPreview = {
	container_id: 'c0ffee',
	name: 'valmin-old-server',
	instance_id: 'old-server',
	base_port: 2456,
	running: true,
	crossplay_instance_id: '',
	game_build_id: '21981590',
	modded: false,
	required_fields: ['server_name', 'world_name', 'password']
};

async function open(global: string[]) {
	daemon.on('GET', '/orphans/c0ffee', () => Response.json(found));
	daemon.on('GET', '/game/options', () => Response.json(gameOptions()));
	daemon.on('GET', '/instances', () => Response.json({ items: [], next_cursor: null }));
	daemon.on('GET', '/me/permissions', () => Response.json(permissions('', [], global)));
	session.permissions = permissions('', [], global);
	render(Page);
}

async function type(label: string, value: string) {
	await fireEvent.input(screen.getByLabelText(label), { target: { value } });
}

const adopts = () => daemon.requests('POST', '/orphans/c0ffee');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(goto).mockClear();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the orphan adoption screen', () => {
	// F3: the preview is not even asked for without instance.adopt.
	it('asks nothing of the daemon for a member without instance.adopt', async () => {
		await open([actions.create]);
		await Promise.resolve();
		expect(daemon.requests('GET', '/orphans/c0ffee')).toHaveLength(0);
	});

	// The operator is promised, before anything happens, that the container is left alone —
	// and told again while the recovery runs.
	it('promises the existing container and its files are untouched', async () => {
		await open([actions.adopt]);
		await screen.findByText('Recoverable server found');
		expect(text(document.body)).toContain(
			'It never stops, recreates, copies, or changes this server during adoption.'
		);
	});

	// The daemon requires every mutable launch field, since it cannot read them back out of a
	// container it did not record.
	it('sends every launch field the daemon requires', async () => {
		await open([actions.adopt]);
		daemon.on('POST', '/orphans/c0ffee', () =>
			Response.json(job({ instance_id: 'old-server' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		const name = (await screen.findByLabelText('Panel name')) as HTMLInputElement;
		await vi.waitFor(() => expect(name.value, 'the name is suggested').toBe('old-server'));
		await type('Server name', 'Old Vikings');
		await type('World name', 'Midgard');
		await type('Server password', 'correct-horse');
		await click(screen.getByRole('button', { name: 'Recover server' }));

		await vi.waitFor(() => expect(adopts()).toHaveLength(1));
		expect(adopts()[0].body).toEqual({
			name: 'old-server',
			server_name: 'Old Vikings',
			world_name: 'Midgard',
			password: 'correct-horse',
			public: false,
			crossplay: false,
			preset: '',
			modifiers: {},
			extra_args: '',
			mem_limit_mb: 4096,
			cpu_limit: null
		});
	});

	// F4: success is the daemon's job succeeding, and only then does the page move on.
	it('follows the job and goes to the server only once it succeeded', async () => {
		await open([actions.adopt]);
		daemon.on('POST', '/orphans/c0ffee', () =>
			Response.json(job({ instance_id: 'old-server' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () =>
			Response.json(job({ instance_id: 'old-server', message: 'Checking the container' }))
		);

		await screen.findByLabelText('Panel name');
		await type('Server name', 'Old Vikings');
		await type('World name', 'Midgard');
		await type('Server password', 'correct-horse');
		await click(screen.getByRole('button', { name: 'Recover server' }));

		expect(await screen.findByText(/Checking the container/)).toBeTruthy();
		expect(text(document.body)).toContain('The existing container and its files stay in place.');
		expect(goto).not.toHaveBeenCalled();
		socket.push('job.job-1', {
			type: 'job',
			id: 'job-1',
			kind: 'adopt',
			status: 'succeeded',
			progress: 100,
			message: 'Adopted'
		});
		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/instances/old-server'));
		expect(daemon.requests('GET', '/me/permissions'), 'the new grant is read').toHaveLength(1);
	});

	it('holds the submit while the password breaks the rules', async () => {
		await open([actions.adopt]);
		await screen.findByLabelText('Panel name');
		await type('Server name', 'Old Vikings');
		await type('World name', 'Midgard');
		await type('Server password', 'abc');

		expect(screen.getByText('At least 5 characters.')).toBeTruthy();
		expect(
			(screen.getByRole('button', { name: 'Recover server' }) as HTMLButtonElement).disabled
		).toBe(true);
	});
});
