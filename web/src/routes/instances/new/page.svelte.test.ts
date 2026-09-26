import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { actions } from '$lib/api/instances';
import type { ModSummary } from '$lib/api/mods';
import type { Job } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, gameOptions, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function summary(overrides: Partial<ModSummary> = {}): ModSummary {
	return {
		full_name: 'Author-Farming',
		source: 'thunderstore',
		namespace: 'Author',
		name: 'Farming',
		description: 'Crops.',
		latest_version: '2.1.0',
		downloads: 1200,
		rating: 5,
		is_deprecated: false,
		categories: [],
		icon_url: '',
		...overrides
	};
}

/** Answers GET /jobs/{id} with whatever `jobs` holds for that id when it is asked. */
function jobs(state: Record<string, Partial<Job>>) {
	for (const id of Object.keys(state)) {
		daemon.on('GET', `/jobs/${id}`, () => Response.json(job({ job_id: id, ...state[id] })));
	}
}

async function open(global: string[] = [actions.create], catalogue: ModSummary[] = [summary()]) {
	daemon.on('GET', '/game/options', () => Response.json(gameOptions()));
	daemon.on('GET', '/mods/search', () =>
		Response.json({ items: catalogue, next_cursor: null, synced_at: null, registries: [] })
	);
	daemon.on('GET', '/instances', () => Response.json({ items: [], next_cursor: null }));
	session.permissions = permissions('', [], global);
	render(Page);
	await vi.waitFor(() => expect(daemon.requests('GET', '/game/options')).toHaveLength(1));
}

/** A field by its label. A required one is announced as "Name * (required)", so by prefix. */
const field = (label: string) => screen.getByLabelText(new RegExp(`^${label}`));

async function type(label: string, value: string) {
	await fireEvent.input(field(label), { target: { value } });
}

async function fillIn() {
	await type('Panel name', 'friday');
	await type('Server name', 'Friday Vikings');
	await type('World name', 'Midgard');
	await type('Server password', 'correct-horse');
}

const create = () => screen.getByRole('button', { name: /Create/ }) as HTMLButtonElement;
const creates = () => daemon.requests('POST', '/instances');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(goto).mockClear();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the create wizard', () => {
	// 03 §1.3's rules, client-side as a courtesy and rendered beside the field they are about.
	it('marks a password that breaks the rules beside the password, and holds the submit', async () => {
		await open();
		await fillIn();
		await type('Server password', 'abcd');

		const password = field('Server password');
		expect(password.getAttribute('aria-invalid')).toBe('true');
		expect(text(document.getElementById('password-error'))).toBe('At least 5 characters.');
		expect(create().disabled).toBe(true);
	});

	it('refuses a world named like the server', async () => {
		await open();
		await fillIn();
		await type('World name', 'Friday Vikings');

		expect(screen.getByText('The world name must differ from the server name.')).toBeTruthy();
		expect(create().disabled).toBe(true);
	});

	it('renders a field the daemon rejected beside it, and moves focus there', async () => {
		await open();
		daemon.on('POST', '/instances', () =>
			envelope(422, 'validation_failed', 'Some fields are invalid.', [
				{ field: 'name', code: 'taken', message: 'Another server already has this name.' }
			])
		);
		await fillIn();
		await click(create());

		const name = field('Panel name');
		await vi.waitFor(() => expect(name.getAttribute('aria-invalid')).toBe('true'));
		expect(text(document.getElementById('name-error'))).toBe(
			'Another server already has this name.'
		);
		await vi.waitFor(() => expect(document.activeElement).toBe(name));
	});

	// F4, ADR-028: creating answers a job, and the job is what is shown until it finishes.
	it('shows the provisioning job and leaves only once it has succeeded', async () => {
		await open();
		daemon.on('POST', '/instances', () =>
			Response.json(job({ instance_id: 'inst-new' }), { status: 202 })
		);
		jobs({ 'job-1': { status: 'running', progress: 30, message: 'Copying game files' } });
		await fillIn();
		await click(create());

		expect(await screen.findByText(/Copying game files/)).toBeTruthy();
		expect(creates()[0].body).toMatchObject({
			name: 'friday',
			server_name: 'Friday Vikings',
			world_name: 'Midgard',
			password: 'correct-horse',
			start_after_provision: true
		});
		expect(goto).not.toHaveBeenCalled();

		const { socket } = await import('$lib/testing/socket');
		socket.push('job.job-1', {
			type: 'job',
			id: 'job-1',
			kind: 'provision',
			status: 'succeeded',
			progress: 100,
			message: 'Ready'
		});
		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/'));
	});

	it('stays on the job when provisioning fails', async () => {
		await open();
		daemon.on('POST', '/instances', () => Response.json(job(), { status: 202 }));
		jobs({ 'job-1': { status: 'failed', error: 'The game download failed.' } });
		await fillIn();
		await click(create());

		expect(await screen.findByText('The game download failed.')).toBeTruthy();
		expect(daemon.requests('GET', '/instances'), 'leaving starts with this read').toHaveLength(0);
		expect(goto).not.toHaveBeenCalled();
	});
});

// Q42: mods are chosen here because the wizard can start the server itself and the world is
// written on that first boot.
describe('the create wizard’s mods', () => {
	it('sends each chosen mod with its version and registry, and says when they go on', async () => {
		await open();
		daemon.on('POST', '/instances', () => Response.json(job(), { status: 202 }));
		jobs({ 'job-1': {} });
		await fillIn();

		await click(await screen.findByRole('button', { name: /Add/ }));
		expect(text(document.body)).toMatch(/before the server first starts/);
		await click(create());

		await vi.waitFor(() => expect(creates()).toHaveLength(1));
		expect((creates()[0].body as { mods: unknown }).mods).toEqual([
			{ full_name: 'Author-Farming', version: '2.1.0', source: 'thunderstore' }
		]);
	});

	it('sends no mods when none were chosen', async () => {
		await open();
		daemon.on('POST', '/instances', () => Response.json(job(), { status: 202 }));
		jobs({ 'job-1': {} });
		await fillIn();
		await click(create());

		await vi.waitFor(() => expect(creates()).toHaveLength(1));
		expect(creates()[0].body).not.toHaveProperty('mods');
	});

	// One package on both registries is two rows; keyed on the name alone, Svelte throws.
	// The wizard searches every registry and offers no switch.
	it('lists a package carried by both registries as two rows, with no registry switch', async () => {
		await open([actions.create], [summary(), summary({ source: 'hexium' })]);

		expect(await screen.findAllByRole('button', { name: /Add/ })).toHaveLength(2);
		expect(screen.queryByRole('group', { name: 'Mod registry' })).toBeNull();
	});
});

describe('the create wizard can start from an existing world', () => {
	const withWorld = [actions.create, actions.worldImport];

	async function pickWorld() {
		const input = screen.getByLabelText('Or files') as HTMLInputElement;
		Object.defineProperty(input, 'files', {
			value: [new File(['x'], 'Midgard.db'), new File(['y'], 'Midgard.fwl')],
			writable: true,
			configurable: true
		});
		await fireEvent.change(input);
	}

	const finish = async (id: string) => {
		const { socket } = await import('$lib/testing/socket');
		socket.push(`job.${id}`, {
			type: 'job',
			id,
			kind: 'x',
			status: 'succeeded',
			progress: 100,
			message: 'Done'
		});
	};

	// F3: at create time the gate is the global list, which the daemon fills for whoever may
	// create a server at all.
	it('offers the world picker only to a holder of world.import', async () => {
		await open([actions.create]);
		expect(screen.queryByLabelText('World folder')).toBeNull();
	});

	it('creating without a world keeps the start and goes straight to the list', async () => {
		await open(withWorld);
		daemon.on('POST', '/instances', () =>
			Response.json(job({ instance_id: 'inst-new' }), { status: 202 })
		);
		jobs({ 'job-1': {} });
		await fillIn();
		await click(create());
		await finish('job-1');

		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/'));
		expect(creates()[0].body).toMatchObject({ start_after_provision: true });
		expect(daemon.requests('POST', '/instances/inst-new/worlds/import')).toHaveLength(0);
	});

	// C19: an import goes into a stopped server, so the chained start is withheld and sent
	// after the import instead — into the instance the daemon's job named.
	it('imports into the new server once it exists, then starts it', async () => {
		await open(withWorld);
		daemon.on('POST', '/instances', () =>
			Response.json(job({ instance_id: 'inst-new' }), { status: 202 })
		);
		daemon.on('POST', '/instances/inst-new/worlds/import', () =>
			Response.json(job({ job_id: 'job-2' }), { status: 202 })
		);
		daemon.on('POST', '/instances/inst-new/start', () =>
			Response.json(job({ job_id: 'job-3' }), { status: 202 })
		);
		jobs({ 'job-1': {}, 'job-2': { message: 'Checking the upload' } });
		await fillIn();
		await pickWorld();

		expect(screen.getByTestId('start-after-import')).toBeTruthy();
		await click(create());
		expect(creates()[0].body).toMatchObject({ start_after_provision: false });
		expect(daemon.requests('POST', '/instances/inst-new/worlds/import')).toHaveLength(0);

		await finish('job-1');
		expect(await screen.findByText(/Checking the upload/)).toBeTruthy();
		expect(daemon.requests('POST', '/instances/inst-new/start')).toHaveLength(0);

		await finish('job-2');
		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/'));
		expect(daemon.requests('POST', '/instances/inst-new/start')).toHaveLength(1);
	});

	// F4: the server exists either way, and a redirect would make a world that did not
	// arrive look like one that did.
	it('keeps the operator on the page when the import fails, with a way to the server', async () => {
		await open(withWorld);
		daemon.on('POST', '/instances', () =>
			Response.json(job({ instance_id: 'inst-new' }), { status: 202 })
		);
		daemon.on('POST', '/instances/inst-new/worlds/import', () =>
			Response.json(job({ job_id: 'job-2' }), { status: 202 })
		);
		jobs({ 'job-1': {}, 'job-2': { status: 'failed', error: 'Found no world in the upload.' } });
		await fillIn();
		await pickWorld();
		await click(create());
		await finish('job-1');

		expect(await screen.findByText('Found no world in the upload.')).toBeTruthy();
		expect(goto).not.toHaveBeenCalled();
		expect(daemon.requests('POST', '/instances/inst-new/start')).toHaveLength(0);
		const link = screen.getByRole('link', { name: 'Go to friday' });
		expect(link.getAttribute('href')).toBe('/instances/inst-new');
	});

	it('imports nothing when provisioning fails', async () => {
		await open(withWorld);
		daemon.on('POST', '/instances', () =>
			Response.json(job({ instance_id: 'inst-new' }), { status: 202 })
		);
		jobs({ 'job-1': { status: 'failed', error: 'No space left.' } });
		await fillIn();
		await pickWorld();
		await click(create());

		expect(await screen.findByText('No space left.')).toBeTruthy();
		expect(daemon.requests('POST', '/instances/inst-new/worlds/import')).toHaveLength(0);
		expect(within(document.body).queryByText(/Importing the world/)).toBeNull();
	});
});
