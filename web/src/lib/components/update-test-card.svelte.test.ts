import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance, type UpdateStatus, type UpdateTest } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import UpdateTestCard from './update-test-card.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

const admin = [actions.view, actions.clone, actions.gameUpdate, actions.modsManage];

function status(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
	return {
		installed_build_id: '21981590',
		public_build_id: '22110001',
		branch: 'public',
		branch_build_id: '22110001',
		observed_at: '2026-09-24T08:00:00Z',
		update_available: true,
		...overrides
	};
}

function report(overrides: Partial<UpdateTest> = {}): UpdateTest {
	return {
		copy_id: 'copy-1',
		copy_name: 'inst-a update test',
		copy_state: 'running',
		copy_port: 2461,
		copy_mem_limit_mb: 4096,
		copy_cpu_limit: 2,
		branch: 'public',
		source_build_id: '21981590',
		copy_build_id: '22110001',
		applicable: true,
		mod_changes: [{ full_name: 'Author-Mod', from: '1.0.0', to: '1.1.0', source: 'thunderstore' }],
		failed: [],
		not_seen: [],
		plugin_load: { observed_at: '2026-09-24T09:00:00Z', declared: 3, loaded: 3, failed: 0 },
		...overrides
	};
}

/** Renders the card for a server, with the daemon answering the copy it is shown. */
async function open(
	held: string[],
	copy: UpdateTest | null,
	row: Instance = instance({ modded: true })
) {
	daemon.on('GET', '/instances/inst-a/update-test', () => Response.json({ test: copy }));
	session.permissions = permissions('inst-a', held);
	render(UpdateTestCard, { instance: row });
	await vi.waitFor(() =>
		expect(daemon.requests('GET', '/instances/inst-a/update-test')).toHaveLength(1)
	);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/instances/inst-a/update-status', () => Response.json(status()));
	daemon.on('GET', '/instances/inst-a/jobs', () => Response.json({ items: [], next_cursor: null }));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the update test card', () => {
	it('offers a test of both the game build and the mods on a modded server', async () => {
		daemon.on('POST', '/instances/inst-a/update-test', () =>
			Response.json(job({ kind: 'update_test' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ kind: 'update_test' })));
		await open(admin, null);

		await click(await screen.findByRole('button', { name: 'Test on a copy' }));

		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/update-test')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/update-test')[0].body).toEqual({
			game: true,
			mods: true
		});
	});

	it('does not offer the game build when the server already has the newest one', async () => {
		daemon.on('GET', '/instances/inst-a/update-status', () =>
			Response.json(status({ update_available: false }))
		);
		await open(admin, null);

		const game = (await screen.findByLabelText(/Install the new game build/)) as HTMLInputElement;
		await vi.waitFor(() => expect(game.checked).toBe(false));
		expect(screen.getByText(/already has the newest public build/)).toBeTruthy();
	});

	// Nobody who cannot use the card asks the daemon about it.
	it('shows and asks for nothing on a vanilla server', async () => {
		session.permissions = permissions('inst-a', admin);
		render(UpdateTestCard, { instance: instance({ modded: false }) });

		await tick();
		expect(screen.queryByText('Test an update on a copy')).toBeNull();
		expect(daemon.requests('GET', '/instances/inst-a/update-test')).toHaveLength(0);
	});

	it('shows and asks for nothing without the permissions the test needs', async () => {
		session.permissions = permissions('inst-a', [actions.view, actions.gameUpdate]);
		render(UpdateTestCard, { instance: instance({ modded: true }) });

		await tick();
		expect(screen.queryByText('Test an update on a copy')).toBeNull();
		expect(daemon.requests('GET', '/instances/inst-a/update-test')).toHaveLength(0);
	});

	// The report is the product: what changed, and which mods did not load.
	it('reports the builds, the mod changes and a mod that failed to load', async () => {
		await open(
			admin,
			report({
				failed: [{ full_name: 'Author-Mod', load_error: 'Could not load [Mod 1.1.0]' }],
				plugin_load: { observed_at: '2026-09-24T09:00:00Z', declared: 3, loaded: 3, failed: 1 }
			})
		);

		expect(await screen.findByText('inst-a update test')).toBeTruthy();
		const card = document.getElementById('update-test')!;
		expect(text(card)).toContain('21981590 becomes 22110001');
		expect(text(card)).toContain('1 mod updates on the copy');
		expect(text(card)).toContain('1.0.0 to 1.1.0');
		expect(text(screen.getByTestId('boot-result'))).toContain('1 mod failed to load on the copy');
		expect(text(screen.getByTestId('boot-result'))).toContain('Could not load [Mod 1.1.0]');
	});

	it('says all mods loaded when none failed', async () => {
		await open(admin, report());
		expect(await screen.findByText(/3 mods loaded on the copy and none failed/)).toBeTruthy();
	});

	it('does not call a copy with no loader log a success', async () => {
		await open(admin, report({ plugin_load: null }));
		expect(await screen.findByText(/has not written a mod loader log/)).toBeTruthy();
		expect(screen.queryByText(/none failed/)).toBeNull();
	});

	it('applies only after the confirmation that names what happens', async () => {
		daemon.on('POST', '/instances/inst-a/update-test/apply', () =>
			Response.json(job({ kind: 'update_apply' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ kind: 'update_apply' })));
		await open(
			admin,
			report({ copy_state: 'stopped' }),
			instance({ modded: true, state: 'running' })
		);

		await click(await screen.findByRole('button', { name: 'Apply to inst-a' }));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('The server is stopped, with its world saved.');
		expect(text(dialog)).toContain('The server is started again.');
		expect(text(dialog)).toContain('The copy is deleted.');
		expect(daemon.requests('POST', '/instances/inst-a/update-test/apply')).toHaveLength(0);

		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'inst-a' }
		});
		await click(within(dialog).getByRole('button', { name: 'Apply' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/update-test/apply')).toHaveLength(1)
		);
	});

	it('discards the copy with its world, once it is stopped', async () => {
		daemon.on('DELETE', '/instances/copy-1', () =>
			Response.json(job({ kind: 'delete' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ kind: 'delete' })));
		await open(admin, report({ copy_state: 'stopped' }));

		await click(await screen.findByRole('button', { name: 'Discard the copy' }));
		const dialog = await screen.findByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'inst-a update test' }
		});
		await click(within(dialog).getByRole('button', { name: 'Discard' }));

		await vi.waitFor(() => expect(daemon.requests('DELETE', '/instances/copy-1')).toHaveLength(1));
		expect(daemon.requests('DELETE', '/instances/copy-1')[0].query.get('keep_worlds')).toBe(
			'false'
		);
	});

	it('asks for the running copy to be stopped before it can be discarded', async () => {
		await open(admin, report({ copy_state: 'running' }));

		expect(await screen.findByRole('button', { name: 'Stop the copy' })).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Discard the copy' })).toBeNull();
	});

	// A test that failed or was cancelled may have left the copy with part of the update.
	it('holds apply back until the copy is ready and its test ran to the end', async () => {
		await open(
			admin,
			report({ copy_state: 'stopped', applicable: false }),
			instance({ modded: true })
		);

		const apply = (await screen.findByRole('button', {
			name: 'Apply to inst-a'
		})) as HTMLButtonElement;
		expect(apply.disabled).toBe(true);
	});

	it('keeps the choices made when the server changes state', async () => {
		daemon.on('GET', '/instances/inst-a/update-test', () => Response.json({ test: null }));
		session.permissions = permissions('inst-a', admin);
		const view = render(UpdateTestCard, { instance: instance({ modded: true }) });
		const game = (await screen.findByLabelText(/Install the new game build/)) as HTMLInputElement;
		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/instances/inst-a/update-test')).toHaveLength(1)
		);

		await click(game);
		expect(game.checked).toBe(false);
		await view.rerender({ instance: instance({ modded: true, state: 'running' }) });
		await tick();

		expect(game.checked).toBe(false);
		expect(daemon.requests('GET', '/instances/inst-a/update-test')).toHaveLength(1);
	});

	it('asks for the changelog of the registry the copy took the mod from', async () => {
		daemon.on('GET', '/instances/inst-a/mods/Author-Mod/changelog', () =>
			Response.json({ version: '1.1.0', text: 'fixes', truncated: false, registry_url: '' })
		);
		await open(
			admin,
			report({
				mod_changes: [{ full_name: 'Author-Mod', from: '1.0.0', to: '1.1.0', source: 'hexium' }]
			})
		);

		await click(await screen.findByText('Changelog'));

		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/instances/inst-a/mods/Author-Mod/changelog')).toHaveLength(1)
		);
		expect(
			daemon.requests('GET', '/instances/inst-a/mods/Author-Mod/changelog')[0].query.get('source')
		).toBe('hexium');
	});

	it('follows a test that is still running after a reload', async () => {
		daemon.on('GET', '/instances/inst-a/jobs', () =>
			Response.json({
				items: [job({ job_id: 'job-7', kind: 'update_test', status: 'running' })],
				next_cursor: null
			})
		);
		daemon.on('GET', '/jobs/job-7', () =>
			Response.json(job({ job_id: 'job-7', kind: 'update_test', message: 'copying the server' }))
		);
		await open(admin, null);

		expect(await screen.findByText(/copying the server/)).toBeTruthy();
	});

	it('tells a test copy where it came from instead of offering its own test', async () => {
		daemon.on('GET', '/instances/copy-1/update-test', () => Response.json({ test: null }));
		session.permissions = permissions('copy-1', admin);
		render(UpdateTestCard, {
			instance: instance({ id: 'copy-1', modded: true, test_of: 'inst-a' })
		});

		expect(await screen.findByText('This is a copy made to test an update')).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Test on a copy' })).toBeNull();
		expect(daemon.requests('GET', '/instances/copy-1/update-test')).toHaveLength(0);
	});
});
