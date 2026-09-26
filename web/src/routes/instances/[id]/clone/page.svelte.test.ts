import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { actions, type Instance } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/clone') }
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

async function open(held: string[], row: Instance = instance()) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', '/instances', () => Response.json({ items: [], next_cursor: null }));
	daemon.on('GET', '/me/permissions', () => Response.json(permissions('inst-a', held)));
	session.permissions = permissions('inst-a', held);
	render(Page);
	await vi.waitFor(() => expect(daemon.requests('GET', '/instances/inst-a')).toHaveLength(1));
}

const cloneButton = () => screen.getByRole('button', { name: 'Clone server' }) as HTMLButtonElement;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(goto).mockClear();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the clone screen', () => {
	it('is closed to a member without instance.clone', async () => {
		await open([actions.view]);
		expect(screen.getByText('Cloning servers is an administrator capability.')).toBeTruthy();
	});

	// A clone copies a world at rest. The panel never stops the source to get one.
	it('refuses a running source, and says it will not stop it for you', async () => {
		await open([actions.view, actions.clone], instance({ state: 'running' }));

		expect(await screen.findByText('Stop inst-a first')).toBeTruthy();
		expect(
			screen.getByText('Cloning never disconnects players or stops the source server for you.')
		).toBeTruthy();
		expect(cloneButton().disabled).toBe(true);
	});

	it('says what is copied, what is fresh, and what stays separate', async () => {
		await open([actions.view, actions.clone]);
		await screen.findByLabelText('New panel name');
		const page = text(document.body);
		expect(page).toContain(
			'The world, installed game build, mods, settings files, launch settings, and game password.'
		);
		expect(page).toContain('its own ports, identity, data directories, and stopped container');
		expect(page).toContain('Users, access grants, and the source backup catalogue are not copied.');
	});

	// F4: the copy exists once the daemon's job says so, and the page then goes to it.
	it('clones under the name given and follows the job to the new server', async () => {
		await open([actions.view, actions.clone]);
		daemon.on('POST', '/instances/inst-a/clone', () =>
			Response.json(job({ instance_id: 'inst-b' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () =>
			Response.json(job({ instance_id: 'inst-b', message: 'Copying the world' }))
		);

		const name = (await screen.findByLabelText('New panel name')) as HTMLInputElement;
		expect(name.value, 'a name is suggested').toBe('inst-a copy');
		await fireEvent.input(name, { target: { value: '  friday-test  ' } });
		await click(cloneButton());

		expect(await screen.findByText(/Copying the world/)).toBeTruthy();
		expect(daemon.requests('POST', '/instances/inst-a/clone')[0].body).toEqual({
			name: 'friday-test'
		});
		expect(goto).not.toHaveBeenCalled();

		socket.push('job.job-1', {
			type: 'job',
			id: 'job-1',
			kind: 'clone',
			status: 'succeeded',
			progress: 100,
			message: 'Done'
		});
		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/instances/inst-b'));
	});

	it('stays on the job when the clone fails', async () => {
		await open([actions.view, actions.clone]);
		daemon.on('POST', '/instances/inst-a/clone', () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () =>
			Response.json(job({ status: 'failed', instance_id: 'inst-b', error: 'No port range left.' }))
		);

		await screen.findByLabelText('New panel name');
		await click(cloneButton());
		expect(await screen.findByText('No port range left.')).toBeTruthy();
		// Leaving starts by re-reading the list and the grants, synchronously with the job's
		// end; neither having been asked for is the page staying put, not a race.
		expect(daemon.requests('GET', '/instances')).toHaveLength(0);
		expect(daemon.requests('GET', '/me/permissions')).toHaveLength(0);
		expect(goto).not.toHaveBeenCalled();
	});
});
