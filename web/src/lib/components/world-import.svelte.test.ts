import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { click } from '$lib/testing/interact';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import WorldImport from './world-import.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const upload = '/instances/inst-a/worlds/import';
let daemon: FakeDaemon;

function open(held: string[], row: Instance = instance()) {
	session.permissions = permissions('inst-a', held);
	render(WorldImport, { instance: row });
}

/** A file as a browser's folder picker hands it over: its path inside the folder rides along. */
function fromFolder(path: string): File {
	const file = new File(['bytes'], path.split('/').at(-1) ?? path);
	Object.defineProperty(file, 'webkitRelativePath', { value: path });
	return file;
}

/** What a file input holds after the operator picked `files`, as bind:files reads it. */
async function pick(label: string, files: File[]) {
	const input = screen.getByLabelText(label) as HTMLInputElement;
	Object.defineProperty(input, 'files', { value: files, writable: true, configurable: true });
	await fireEvent.change(input);
}

async function confirmImport(world = 'MyWorld') {
	await click(screen.getByRole('button', { name: 'Import world' }));
	const dialog = await screen.findByRole('dialog');
	const confirm = within(dialog).getByRole('button', { name: 'Import world' });
	expect((confirm as HTMLButtonElement).disabled, 'not before the name is typed').toBe(true);
	await fireEvent.input(within(dialog).getByLabelText(/to confirm/), { target: { value: world } });
	await click(confirm);
	return dialog;
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('POST', upload, () => Response.json(job(), { status: 202 }));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job({ message: 'Archiving the old world' })));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the world import panel', () => {
	// F3: world.import is its own capability, separate from everything else on the screen.
	it('is closed to a member without world.import', () => {
		open([actions.settings]);
		expect(screen.getByText('Importing a world is not available to you.')).toBeTruthy();
		expect(screen.queryByLabelText('World folder')).toBeNull();
	});

	// C19: the daemon refuses an import into anything but a stopped server; the panel says so
	// before the click.
	it('says a running server must be stopped, and offers nothing to pick', () => {
		open([actions.worldImport], instance({ state: 'running' }));

		expect(screen.getByText('This server is running. Stop it to import a world.')).toBeTruthy();
		expect((screen.getByLabelText('World folder') as HTMLInputElement).disabled).toBe(true);
		expect(
			(screen.getByRole('button', { name: 'Import world' }) as HTMLButtonElement).disabled
		).toBe(true);
	});

	// 03 §4, ADR-180: a 1.0 world is a folder whose name is the world's name, so each file's
	// path inside the folder is what gets sent, not the leaf name alone.
	it('uploads a picked folder with each file’s path, after the world name is typed back', async () => {
		open([actions.worldImport]);
		await pick('World folder', [
			fromFolder('Midgard/Midgard.db2'),
			fromFolder('Midgard/Midgard.fwl')
		]);

		const dialog = await confirmImport();
		expect(dialog.textContent).toContain('Replace MyWorld?');
		await vi.waitFor(() => expect(daemon.requests('POST', upload)).toHaveLength(1));

		const sent = daemon.requests('POST', upload)[0];
		const names = (sent.body as FormData).getAll('file').map((f) => (f as File).name);
		expect(names).toEqual(['Midgard/Midgard.db2', 'Midgard/Midgard.fwl']);
		expect(sent.query.get('allow_backup_variant')).toBe('false');
	});

	it('sends nothing when the confirmation is cancelled', async () => {
		open([actions.worldImport]);
		await pick('Or files', [new File(['x'], 'MyWorld.db'), new File(['y'], 'MyWorld.fwl')]);

		await click(screen.getByRole('button', { name: 'Import world' }));
		await click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
		expect(daemon.requests('POST', upload)).toHaveLength(0);
	});

	// 03 §4.1 rule 5: a game's rolling backup is refused unless the operator says it is meant.
	it('asks for an older game backup explicitly rather than inferring it', async () => {
		open([actions.worldImport]);
		await pick('Or files', [new File(['x'], 'MyWorld.db.old'), new File(['y'], 'MyWorld.fwl.old')]);
		await click(screen.getByRole('switch', { name: 'Import an older game backup' }));

		await confirmImport();
		await vi.waitFor(() => expect(daemon.requests('POST', upload)).toHaveLength(1));
		expect(daemon.requests('POST', upload)[0].query.get('allow_backup_variant')).toBe('true');
	});

	// F2: what counts as a world is the daemon's rule. A filter here would hide files that
	// rule accepts, and the refusal arrives as the job's own error.
	it('filters nothing, and shows the daemon’s refusal as it was sent', async () => {
		open([actions.worldImport]);
		for (const label of ['World folder', 'Or files']) {
			expect(screen.getByLabelText(label).hasAttribute('accept'), label).toBe(false);
		}
		daemon.on('GET', '/jobs/job-1', () =>
			Response.json(job({ status: 'failed', error: 'Found 2 worlds in the upload: A, B.' }))
		);
		await pick('Or files', [new File(['x'], 'A.db'), new File(['y'], 'B.db')]);

		await confirmImport();
		expect(await screen.findByText('Found 2 worlds in the upload: A, B.')).toBeTruthy();
	});

	// F4: the panel shows the job the daemon reports, and holds the gate while it runs.
	it('follows the import job and holds the gate while it runs', async () => {
		open([actions.worldImport]);
		await pick('Or files', [new File(['x'], 'MyWorld.db'), new File(['y'], 'MyWorld.fwl')]);

		await confirmImport();
		expect(await screen.findByText(/Archiving the old world/)).toBeTruthy();
		expect(screen.getByText('An import is running. Wait for it to finish.')).toBeTruthy();
	});
});
