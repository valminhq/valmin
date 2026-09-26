import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance } from '$lib/api/instances';
import type {
	InstalledMod,
	ModSummary,
	PluginLoad,
	ResolvedNode,
	UpdatePreview
} from '$lib/api/mods';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import { choose, click, text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/mods') }
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const base = '/instances/inst-a/mods';
let daemon: FakeDaemon;

function installed(overrides: Partial<InstalledMod> = {}): InstalledMod {
	return {
		full_name: 'Author-Sailing',
		source: 'thunderstore',
		namespace: 'Author',
		name: 'Sailing',
		version: '1.0.0',
		installed_as: 'explicit',
		update_version: '',
		is_deprecated: false,
		not_indexed: false,
		side: 'unknown',
		enabled: true,
		installed_at: '2026-09-01T00:00:00Z',
		file_count: 2,
		load_status: 'loaded',
		load_error: null,
		...overrides
	};
}

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

function node(overrides: Partial<ResolvedNode> = {}): ResolvedNode {
	return {
		full_name: 'Author-Farming',
		source: 'thunderstore',
		version: '2.1.0',
		transitive: false,
		no_op: false,
		...overrides
	};
}

const boot: PluginLoad = {
	observed_at: '2026-09-20T12:00:00Z',
	declared: 1,
	loaded: 1,
	failed: 0,
	discrepancy: null
};

/** Renders the mod screen for a member holding `held`, once the installed list has loaded. */
async function open(
	held: string[],
	{
		mods = [installed()],
		row = instance(),
		catalogue = [summary()],
		load = boot
	}: {
		mods?: InstalledMod[];
		row?: Instance;
		catalogue?: ModSummary[];
		load?: PluginLoad | null;
	} = {}
) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', base, () => Response.json({ mods, plugin_load: load }));
	daemon.on('GET', `${base}/export`, () =>
		Response.json({ profile_name: 'p', mods: [], excluded: [], conflicts: [] })
	);
	daemon.on('GET', '/mods/search', () =>
		Response.json({
			items: catalogue,
			next_cursor: null,
			synced_at: '2026-09-20T12:00:00Z',
			registries: [
				{ source: 'thunderstore', enabled: true, synced_at: '2026-09-20T12:00:00Z' },
				{ source: 'hexium', enabled: true, synced_at: '2026-09-20T12:00:00Z' }
			]
		})
	);
	session.permissions = permissions('inst-a', held);
	render(Page);
	await screen.findByRole('tab', { name: /Installed \d/ });
}

async function browse() {
	await click(screen.getByRole('tab', { name: 'Browse mods' }));
	await screen.findAllByText('Farming');
}

const button = (name: string | RegExp) => screen.getByRole('button', { name });
const disabled = (el: HTMLElement) => (el as HTMLButtonElement).disabled;
const manage = [actions.modsList, actions.modsManage];

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the mod screen', () => {
	// Q38. `failed` is the loader saying it could not load a plugin; the screen names the mod
	// and shows the loader's own line. `not_seen` stays distinct: nothing said it failed.
	it('names a mod that failed to load, with the loader’s reason', async () => {
		await open([actions.modsList], {
			mods: [installed({ load_status: 'failed', load_error: 'TypeLoadException: Jotunn.Manager' })]
		});

		const alert = screen.getByRole('alert');
		expect(text(alert)).toContain('1 of 1 mod failed to load');
		expect(text(alert)).toContain('Author-Sailing: TypeLoadException: Jotunn.Manager');
		expect(screen.getByText('failed to load')).toBeTruthy();
	});

	it('keeps a mod that was never seen loading apart from one that failed', async () => {
		await open([actions.modsList], {
			mods: [installed({ load_status: 'not_seen' }), installed({ full_name: 'Author-Other' })]
		});

		expect(text(screen.getByRole('alert'))).toContain('1 of 2 mods did not load');
		expect(screen.getByText('not loading')).toBeTruthy();
		expect(screen.queryByText('failed to load')).toBeNull();
	});

	// Q39: a package its own registry no longer lists has no update and no deprecation flag to
	// read, so the screen states that rather than showing a row with nothing to say.
	it('says when a mod is no longer in its registry’s index', async () => {
		await open([actions.modsList], { mods: [installed({ not_indexed: true })] });

		expect(screen.getByText('not in the index')).toBeTruthy();
		expect(screen.getByText(/Thunderstore no longer lists this mod/)).toBeTruthy();
	});

	it('says everything loaded only when the load report says so', async () => {
		await open([actions.modsList]);
		expect(screen.getByText('Everything loaded')).toBeTruthy();
		expect(screen.queryByRole('alert')).toBeNull();
	});

	// F3: what is offered comes from the actions the daemon sent.
	it('offers a member who can only list mods no change at all', async () => {
		await open([actions.modsList], { mods: [installed({ update_version: '1.1.0' })] });

		expect(screen.queryByRole('button', { name: /Disable|Remove|Update/ })).toBeNull();
		expect(screen.getByText('Unknown'), 'the side is shown as a label').toBeTruthy();
		await browse();
		expect(screen.queryByRole('button', { name: /Install/ })).toBeNull();
	});

	// B11 / C19. The daemon refuses mod changes on a running server; the screen says why the
	// buttons are dead before the click rather than after it.
	it('disables every file change on a running server and says why', async () => {
		await open(manage, { row: instance({ state: 'running' }) });

		expect(
			screen.getByText('This server is running. Stop it to install or remove mods.')
		).toBeTruthy();
		expect(disabled(button('Disable Author-Sailing'))).toBe(true);
		expect(disabled(button('Remove Author-Sailing'))).toBe(true);
		await browse();
		expect(disabled(button(/Install/))).toBe(true);
	});

	// Q37: the side tag is recorded and nothing on disk reads it, so it stays editable while
	// the server is up — that is when an operator learns what their players need.
	it('keeps the side label editable while the server runs', async () => {
		await open(manage, { row: instance({ state: 'running' }) });

		expect(disabled(screen.getByLabelText('Client requirement'))).toBe(false);
	});

	// 04 §3: resolve before install. The operator agrees to the whole closure before
	// anything downloads, and only the dialog's confirm sends the install.
	it('installs only after the resolved closure is confirmed', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({
				nodes: [node(), node({ full_name: 'Author-Lib', version: '0.3.0', transitive: true })]
			})
		);
		daemon.on('POST', base, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Install Farming?');
		expect(dialog.textContent).toContain('2 packages will be installed or updated.');
		expect(within(dialog).getByText('Author-Lib').parentElement?.textContent).toContain(
			'dependency'
		);
		expect(daemon.requests('POST', `${base}/resolve`)[0].body).toEqual({
			full_name: 'Author-Farming',
			version: '2.1.0',
			source: 'thunderstore'
		});
		expect(daemon.requests('POST', base), 'nothing installs before the confirm').toHaveLength(0);

		await click(within(dialog).getByRole('button', { name: 'Install mod' }));
		await vi.waitFor(() => expect(daemon.requests('POST', base)).toHaveLength(1));
		expect(daemon.requests('POST', base)[0].body).toEqual({
			full_name: 'Author-Farming',
			version: '2.1.0',
			source: 'thunderstore'
		});
	});

	it('sends nothing when the closure is cancelled', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () => Response.json({ nodes: [node()] }));
		await browse();

		await click(button(/Install/));
		await click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
		expect(daemon.requests('POST', base)).toHaveLength(0);
	});

	it('offers no install when everything required is already there', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () => Response.json({ nodes: [node({ no_op: true })] }));
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Everything required is already installed.');
		expect(disabled(within(dialog).getByRole('button', { name: 'Install mod' }))).toBe(true);
	});

	// F4: the list is re-read when the job the daemon reported finishes, not flipped here.
	it('re-reads the installed list when the install job finishes, and not before', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () => Response.json({ nodes: [node()] }));
		daemon.on('POST', base, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ message: 'Downloading' })));
		await browse();

		await click(button(/Install/));
		await click(
			within(await screen.findByRole('dialog')).getByRole('button', { name: 'Install mod' })
		);
		await screen.findByText(/Downloading/);
		expect(daemon.requests('GET', base)).toHaveLength(1);

		socket.push('job.job-1', {
			type: 'job',
			id: 'job-1',
			kind: 'mod_install',
			status: 'succeeded',
			progress: 100,
			message: 'Installed'
		});
		await vi.waitFor(() => expect(daemon.requests('GET', base)).toHaveLength(2));
	});

	// F5: removal names the mod, and orphan removal is the operator's explicit choice.
	it('removes a mod only through the confirmation that names it', async () => {
		await open(manage);
		daemon.on('DELETE', `${base}/Author-Sailing`, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(button('Remove Author-Sailing'));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Remove Author-Sailing?');
		expect(daemon.requests('DELETE', `${base}/Author-Sailing`)).toHaveLength(0);

		await click(within(dialog).getByLabelText(/Remove unused dependencies too/));
		await click(within(dialog).getByRole('button', { name: 'Remove mod' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', `${base}/Author-Sailing`)).toHaveLength(1)
		);
		expect(daemon.requests('DELETE', `${base}/Author-Sailing`)[0].query.get('remove_orphans')).toBe(
			'true'
		);
	});

	// An installed row says when a newer version is known, and says nothing when none is:
	// "no newer version known" and "up to date" are different claims.
	it('offers an update on the installed row when the daemon knows a newer version', async () => {
		await open(manage, { mods: [installed({ update_version: '1.2.0' })] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({ nodes: [node({ full_name: 'Author-Sailing', version: '1.2.0' })] })
		);

		expect(screen.getByText('1.2.0 available')).toBeTruthy();
		await click(button('Update'));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Update Sailing?');
		expect(within(dialog).getByRole('button', { name: 'Update mod' })).toBeTruthy();
		expect(daemon.requests('POST', `${base}/resolve`)[0].body).toMatchObject({ version: '1.2.0' });
	});

	it('claims nothing about a mod with no newer version known', async () => {
		await open(manage);
		expect(screen.queryByText(/^\S+ available$/)).toBeNull();
		expect(screen.queryByRole('button', { name: 'Update' })).toBeNull();
	});

	// Q39: one combined diff from the daemon, confirmed once, with the backup said out loud.
	it('updates all mods through one confirmed diff that says the world is backed up', async () => {
		await open(manage, { mods: [installed({ update_version: '1.2.0' })] });
		const preview: UpdatePreview = {
			targets: [{ full_name: 'Author-Sailing', source: 'thunderstore', version: '1.2.0' }],
			nodes: [
				{
					full_name: 'Author-Sailing',
					source: 'thunderstore',
					from_version: '1.0.0',
					version: '1.2.0',
					transitive: false
				},
				{
					full_name: 'Author-Lib',
					source: 'thunderstore',
					from_version: '',
					version: '0.3.0',
					transitive: true
				}
			],
			backup: true
		};
		daemon.on('POST', `${base}/updates/resolve`, () => Response.json(preview));
		daemon.on('POST', `${base}/updates`, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(button('Update all mods (1)'));
		const dialog = await screen.findByRole('dialog');
		const diff = within(dialog).getByTestId('update-all-diff');
		expect(text(diff)).toContain('1.0.0 → 1.2.0');
		expect(diff.textContent).toContain('new dependency');
		expect(dialog.textContent).toContain('The world is backed up first.');
		expect(daemon.requests('POST', `${base}/updates`)).toHaveLength(0);

		await click(within(dialog).getByRole('button', { name: 'Back up and update' }));
		await vi.waitFor(() => expect(daemon.requests('POST', `${base}/updates`)).toHaveLength(1));
		expect(daemon.requests('POST', `${base}/updates`)[0].body).toEqual({
			targets: preview.targets
		});
	});

	// Q37: disabling moves files, so it is a job like any other change, and the row is not
	// flipped before the daemon has done it.
	it('disables a mod through a job and does not flip the row ahead of it', async () => {
		await open(manage);
		daemon.on('PATCH', `${base}/Author-Sailing`, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ message: 'Moving files' })));

		await click(button('Disable Author-Sailing'));
		await screen.findByText(/Moving files/);
		expect(daemon.requests('PATCH', `${base}/Author-Sailing`)[0].body).toEqual({ enabled: false });
		expect(screen.queryByText('disabled'), 'still enabled until the job says so').toBeNull();
		expect(button('Disable Author-Sailing')).toBeTruthy();
	});

	it('labels a disabled mod and offers to enable it', async () => {
		await open(manage, { mods: [installed({ enabled: false })] });
		expect(screen.getByText('disabled')).toBeTruthy();
		expect(button('Enable Author-Sailing')).toBeTruthy();
	});

	// Q37: a side label is recorded, re-read from the daemon, and never stops the server.
	it('records a side label and re-reads the list', async () => {
		await open(manage, { row: instance({ state: 'running' }) });
		daemon.on('PATCH', `${base}/Author-Sailing`, () =>
			Response.json(installed({ side: 'client_required' }))
		);

		await choose(screen.getByLabelText('Client requirement'), 'Client required');

		await vi.waitFor(() =>
			expect(daemon.requests('PATCH', `${base}/Author-Sailing`)).toHaveLength(1)
		);
		expect(daemon.requests('PATCH', `${base}/Author-Sailing`)[0].body).toEqual({
			side: 'client_required'
		});
		await vi.waitFor(() => expect(daemon.requests('GET', base)).toHaveLength(2));
		expect(
			daemon.sent.some((r) => r.path.endsWith('/stop')),
			'the server is left running'
		).toBe(false);
	});

	// The registry switch must re-run the search: a control that only changes its highlight
	// is the defect most likely to ship.
	it('narrows the catalogue to the registry chosen', async () => {
		await open(manage, { mods: [] });
		await browse();
		const before = daemon.requests('GET', '/mods/search').length;

		const group = screen.getByRole('group', { name: 'Mod registry' });
		await click(within(group).getByRole('button', { name: 'Hexium' }));
		expect(within(group).getByRole('button', { name: 'Hexium' }).getAttribute('aria-pressed')).toBe(
			'true'
		);
		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/mods/search').length).toBeGreaterThan(before)
		);
		expect(daemon.requests('GET', '/mods/search').at(-1)?.query.get('source')).toBe('hexium');
	});

	// A search that answers late must not replace the results of one typed after it.
	it('never lets an older search overwrite a newer one', async () => {
		await open(manage, { mods: [] });
		await browse();
		let release = () => {};
		const page = (name: string) =>
			Response.json({
				items: [summary({ name, full_name: `Author-${name}` })],
				next_cursor: null,
				synced_at: null,
				registries: []
			});
		daemon.on('GET', '/mods/search', (r) =>
			r.query.get('q') === 'old'
				? new Promise<Response>((done) => (release = () => done(page('Stale'))))
				: page('Fresh')
		);

		const box = screen.getByLabelText('Search mods');
		await fireEvent.input(box, { target: { value: 'old' } });
		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/mods/search').some((r) => r.query.get('q') === 'old')).toBe(
				true
			)
		);
		await fireEvent.input(box, { target: { value: 'new' } });
		await screen.findByText('Fresh');

		release();
		// The late answer is already in hand; one macrotask lets its promise chain run to the
		// point where the page either renders it or throws it away.
		await new Promise((done) => setTimeout(done, 0));
		expect(screen.queryByText('Stale')).toBeNull();
		expect(screen.getByText('Fresh')).toBeTruthy();
	});

	// One package on both registries is two rows with one name. Keyed on the name alone,
	// Svelte throws on the duplicate — a crash on a real catalogue.
	it('lists a package carried by both registries as two rows', async () => {
		await open(manage, {
			mods: [],
			catalogue: [summary(), summary({ source: 'hexium' })]
		});
		await browse();

		expect(screen.getAllByText('Farming')).toHaveLength(2);
		const rows = screen.getAllByText('Farming').map((el) => el.closest('li')?.textContent ?? '');
		expect(rows[0]).toContain('Thunderstore');
		expect(rows[1]).toContain('Hexium');
	});
});
