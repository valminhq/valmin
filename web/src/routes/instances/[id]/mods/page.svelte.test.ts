import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance } from '$lib/api/instances';
import type {
	InstalledMod,
	ModSummary,
	PluginLoad,
	QueuedMod,
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
		locked: false,
		is_pack: false,
		pack: '',
		pack_version: '',
		pack_override: false,
		installed_at: '2026-09-01T00:00:00Z',
		file_count: 2,
		config_file_count: 0,
		config_files: [],
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
		from_version: '',
		version: '2.1.0',
		change: 'install',
		transitive: false,
		no_op: false,
		...overrides
	};
}

/** The parts of a resolve answer a plain install leaves empty. */
const noChange = { removals: [], kept: [], conflicts: [] };

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
		load = boot,
		queued = []
	}: {
		mods?: InstalledMod[];
		row?: Instance;
		catalogue?: ModSummary[];
		load?: PluginLoad | null;
		queued?: QueuedMod[];
	} = {}
) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', base, () => Response.json({ mods, plugin_load: load }));
	daemon.on('GET', `${base}/queue`, () => Response.json({ queued }));
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

	it('links a mod to its config files: the editor for one, the searched list for several', async () => {
		await open([actions.modsList, actions.configRead], {
			mods: [
				installed({ config_files: ['Author.Sailing.cfg'] }),
				installed({
					full_name: 'Author-Biomes',
					name: 'Biomes',
					config_files: ['Author.Biomes.Ashlands.cfg', 'Author.Biomes.cfg']
				}),
				installed({ full_name: 'Author-Plain', name: 'Plain' })
			]
		});

		const href = (name: string) => screen.getByRole('link', { name }).getAttribute('href');
		expect(href('Configure Author-Sailing')).toBe('/instances/inst-a/configs/Author.Sailing.cfg');
		expect(href('Configure Author-Biomes')).toBe('/instances/inst-a/configs?q=Author.Biomes.');
		expect(screen.queryByRole('link', { name: 'Configure Author-Plain' })).toBeNull();
	});

	it('offers no configure link without config.read', async () => {
		await open(manage, { mods: [installed({ config_files: ['Author.Sailing.cfg'] })] });

		expect(screen.queryByRole('link', { name: /Configure/ })).toBeNull();
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

	// B11 / C19. The daemon refuses removals and toggles on a running server; the screen says
	// why those buttons are dead before the click, and keeps install, which queues.
	it('disables removals on a running server, says why, and keeps install', async () => {
		await open(manage, { row: instance({ state: 'running' }) });

		expect(screen.getByTestId('mod-actions-blocked').textContent).toContain(
			'Installs and updates wait until it stops or restarts'
		);
		expect(disabled(button('Disable Author-Sailing'))).toBe(true);
		expect(disabled(button('Remove Author-Sailing'))).toBe(true);
		await browse();
		expect(disabled(button(/Install/))).toBe(false);
	});

	// A confirmed install on a running server is queued, never sent as a job.
	it('queues an install on a running server', async () => {
		await open(manage, { mods: [], row: instance({ state: 'running' }) });
		daemon.on('POST', `${base}/resolve`, () => Response.json({ ...noChange, nodes: [node()] }));
		const entry = {
			full_name: 'Author-Farming',
			version: '2.1.0',
			source: 'thunderstore',
			created_at: '2026-10-07T12:00:00Z'
		};
		daemon.on('POST', `${base}/queue`, () => Response.json(entry, { status: 201 }));
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(within(dialog).getByTestId('queue-notice')).toBeTruthy();
		daemon.on('GET', `${base}/queue`, () => Response.json({ queued: [entry] }));
		await click(within(dialog).getByRole('button', { name: 'Install when stopped' }));

		await vi.waitFor(() => expect(daemon.requests('POST', `${base}/queue`)).toHaveLength(1));
		expect(daemon.requests('POST', `${base}/queue`)[0].body).toEqual({
			full_name: 'Author-Farming',
			version: '2.1.0',
			source: 'thunderstore'
		});
		expect(daemon.requests('POST', base), 'no install job on a running server').toHaveLength(0);
		expect((await screen.findByTestId('mod-queue')).textContent).toContain('Author-Farming');
	});

	it('removes a queued install', async () => {
		await open(manage, {
			row: instance({ state: 'running' }),
			queued: [
				{
					full_name: 'Author-Farming',
					version: '2.1.0',
					source: 'thunderstore',
					created_at: '2026-10-07T12:00:00Z'
				}
			]
		});
		daemon.on('DELETE', `${base}/queue/Author-Farming`, () => new Response(null, { status: 204 }));

		await click(button('Remove Author-Farming from the queue'));

		await vi.waitFor(() => expect(screen.queryByTestId('mod-queue')).toBeNull());
		expect(daemon.requests('DELETE', `${base}/queue/Author-Farming`)).toHaveLength(1);
	});

	it('says the queue is installing on a stopped server and re-reads it on a mods signal', async () => {
		await open(manage, {
			row: instance({ state: 'stopped' }),
			queued: [
				{
					full_name: 'Author-Farming',
					version: '2.1.0',
					source: 'thunderstore',
					created_at: '2026-10-07T12:00:00Z'
				}
			]
		});
		expect(screen.getByTestId('mod-queue').textContent).toContain('Installing queued mods');

		daemon.on('GET', `${base}/queue`, () => Response.json({ queued: [] }));
		socket.push('instance.inst-a.state', { type: 'mods', instance: 'inst-a' });

		await vi.waitFor(() => expect(screen.queryByTestId('mod-queue')).toBeNull());
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
				...noChange,
				nodes: [node(), node({ full_name: 'Author-Lib', version: '0.3.0', transitive: true })]
			})
		);
		daemon.on('POST', base, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Install Farming?');
		expect(dialog.textContent).toContain('2 packages will be installed or changed.');
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
		daemon.on('POST', `${base}/resolve`, () => Response.json({ ...noChange, nodes: [node()] }));
		await browse();

		await click(button(/Install/));
		await click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
		expect(daemon.requests('POST', base)).toHaveLength(0);
	});

	it('offers no install when everything required is already there', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({ ...noChange, nodes: [node({ no_op: true })] })
		);
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Everything required is already installed.');
		expect(disabled(within(dialog).getByRole('button', { name: 'Install mod' }))).toBe(true);
	});

	// F4: the list is re-read when the job the daemon reported finishes, not flipped here.
	it('re-reads the installed list when the install job finishes, and not before', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () => Response.json({ ...noChange, nodes: [node()] }));
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

	// F5: removal names the mod, and orphan removal is the operator's explicit choice. The
	// count is of the files removal deletes: config files are left in place.
	it('removes a mod only through the confirmation that names it', async () => {
		await open(manage, { mods: [installed({ file_count: 3, config_file_count: 1 })] });
		daemon.on('DELETE', `${base}/Author-Sailing`, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(button('Remove Author-Sailing'));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Remove Author-Sailing?');
		expect(text(dialog)).toContain('The 2 files it placed are deleted.');
		expect(text(dialog)).toContain('Its config files stay');
		expect(daemon.requests('DELETE', `${base}/Author-Sailing`)).toHaveLength(0);

		await click(within(dialog).getByLabelText(/Remove unused dependencies too/));
		await click(within(dialog).getByRole('button', { name: 'Remove mod' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', `${base}/Author-Sailing`)).toHaveLength(1)
		);
		expect(daemon.requests('DELETE', `${base}/Author-Sailing`)[0].query.get('remove_orphans')).toBe(
			'true'
		);
		expect(daemon.requests('DELETE', `${base}/Author-Sailing`)[0].query.get('remove_configs')).toBe(
			'false'
		);
	});

	it('offers to remove the config files a mod leaves behind', async () => {
		await open([...manage, actions.configEdit], {
			mods: [installed({ leftover_configs: ['Author.Sailing.cfg'] })]
		});
		daemon.on('DELETE', `${base}/Author-Sailing`, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(button('Remove Author-Sailing'));
		const dialog = await screen.findByRole('dialog');
		const offer = within(dialog).getByLabelText(/Remove its config files too/);
		expect(text(dialog)).toContain('Author.Sailing.cfg');

		await click(offer);
		expect(text(dialog)).toContain('Its config files are deleted too.');
		await click(within(dialog).getByRole('button', { name: 'Remove mod' }));
		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', `${base}/Author-Sailing`)).toHaveLength(1)
		);
		expect(daemon.requests('DELETE', `${base}/Author-Sailing`)[0].query.get('remove_configs')).toBe(
			'true'
		);
	});

	it('does not offer config removal without config.edit', async () => {
		await open(manage, { mods: [installed({ leftover_configs: ['Author.Sailing.cfg'] })] });

		await click(button('Remove Author-Sailing'));
		const dialog = await screen.findByRole('dialog');
		expect(within(dialog).queryByLabelText(/Remove its config files too/)).toBeNull();
	});

	// An installed row says when a newer version is known, and says nothing when none is:
	// "no newer version known" and "up to date" are different claims.
	it('offers an update on the installed row when the daemon knows a newer version', async () => {
		await open(manage, { mods: [installed({ update_version: '1.2.0' })] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({
				...noChange,
				nodes: [node({ full_name: 'Author-Sailing', from_version: '1.0.0', version: '1.2.0' })],
				backup: true
			})
		);

		expect(screen.getByText('1.2.0 available')).toBeTruthy();
		await click(button('Update'));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).toContain('Update Sailing?');
		expect(text(dialog)).toContain('1.0.0 → 1.2.0');
		expect(dialog.textContent).toContain('The world is backed up first.');
		expect(within(dialog).getByRole('button', { name: 'Back up and update' })).toBeTruthy();
		expect(daemon.requests('POST', `${base}/resolve`)[0].body).toMatchObject({ version: '1.2.0' });
	});

	it('shows what an install replaces and backs up the world first', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({
				...noChange,
				nodes: [
					node(),
					node({
						full_name: 'denikson-BepInExPack_Valheim',
						from_version: '5.4.2200',
						version: '5.4.2333',
						transitive: true
					}),
					node({ full_name: 'Author-Lib', version: '0.3.0', transitive: true })
				],
				backup: true
			})
		);
		daemon.on('POST', base, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		const framework = text(within(dialog).getByText('denikson-BepInExPack_Valheim').parentElement);
		expect(framework).toContain('5.4.2200 → 5.4.2333');
		expect(framework).toContain('update');
		expect(framework).not.toContain('dependency');
		expect(within(dialog).getByText('Author-Lib').parentElement?.textContent).toContain(
			'dependency'
		);
		expect(text(dialog)).toContain(
			'The world is backed up first. The backup is kept even if the install fails.'
		);

		await click(within(dialog).getByRole('button', { name: 'Back up and install' }));
		await vi.waitFor(() => expect(daemon.requests('POST', base)).toHaveLength(1));
	});

	it('says nothing about a backup when the install replaces nothing', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({ ...noChange, nodes: [node()], backup: false })
		);
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent).not.toContain('backed up');
		expect(within(dialog).getByRole('button', { name: 'Install mod' })).toBeTruthy();
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
					change: 'upgrade',
					transitive: false
				},
				{
					full_name: 'Author-Lib',
					source: 'thunderstore',
					from_version: '',
					version: '0.3.0',
					change: 'install',
					transitive: true
				}
			],
			conflicts: [],
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

	it('queues every update in one request on a running server instead of starting a job', async () => {
		await open(manage, {
			mods: [installed({ update_version: '1.2.0' })],
			row: instance({ state: 'running' })
		});
		const preview: UpdatePreview = {
			targets: [
				{ full_name: 'Author-Sailing', source: 'thunderstore', version: '1.2.0' },
				{ full_name: 'Author-Lib', source: 'thunderstore', version: '0.4.0' }
			],
			nodes: [],
			conflicts: [],
			backup: true
		};
		const queued = preview.targets.map((t) => ({ ...t, created_at: '2026-10-08T12:00:00Z' }));
		daemon.on('POST', `${base}/updates/resolve`, () => Response.json(preview));
		daemon.on('POST', `${base}/updates/queue`, () => Response.json({ queued }, { status: 201 }));

		await click(button('Update all mods (1)'));
		const dialog = await screen.findByRole('dialog');
		expect(within(dialog).getByTestId('update-all-queue-notice')).toBeTruthy();
		await click(within(dialog).getByRole('button', { name: 'Update when stopped' }));

		await vi.waitFor(() =>
			expect(daemon.requests('POST', `${base}/updates/queue`)).toHaveLength(1)
		);
		expect(daemon.requests('POST', `${base}/updates/queue`)[0].body).toEqual({
			targets: preview.targets
		});
		expect(daemon.requests('POST', `${base}/queue`), 'no per-mod queue requests').toHaveLength(0);
		expect(
			daemon.requests('POST', `${base}/updates`),
			'no update job on a running server'
		).toHaveLength(0);
		expect((await screen.findByTestId('mod-queue')).textContent).toContain('Author-Lib');
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

	it('downgrades to a version picked in the dialog and says what a downgrade leaves alone', async () => {
		await open(manage, { mods: [installed({ version: '1.2.0' })] });
		daemon.on('GET', '/mods/Author/Sailing', () =>
			Response.json({
				...summary({ full_name: 'Author-Sailing', name: 'Sailing' }),
				versions: [
					{ version: '1.2.0', source: 'thunderstore', dependencies: [] },
					{ version: '1.0.0', source: 'thunderstore', dependencies: [] }
				]
			})
		);
		daemon.on('POST', `${base}/resolve`, (request) => {
			const version = (request.body as { version: string }).version;
			const sailing = { full_name: 'Author-Sailing', from_version: '1.2.0', version };
			return Response.json({
				...noChange,
				nodes: [
					version === '1.2.0'
						? node({ ...sailing, change: 'none', no_op: true })
						: node({ ...sailing, change: 'downgrade' })
				]
			});
		});
		daemon.on('POST', base, () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));

		await click(button('Change the version of Author-Sailing'));
		const dialog = await screen.findByRole('dialog');
		expect(daemon.requests('POST', `${base}/resolve`)[0].body).toMatchObject({ version: '1.2.0' });
		await choose(await within(dialog).findByLabelText('Version'), '1.0.0');

		await within(dialog).findByText('Downgrade Sailing?');
		expect(text(dialog)).toContain('1.2.0 → 1.0.0');
		expect(text(dialog)).toContain('It does not restore the world or config files');
		await click(within(dialog).getByRole('button', { name: 'Downgrade mod' }));
		await vi.waitFor(() => expect(daemon.requests('POST', base)).toHaveLength(1));
		expect(daemon.requests('POST', base)[0].body).toEqual({
			full_name: 'Author-Sailing',
			version: '1.0.0',
			source: 'thunderstore'
		});
	});

	it('names what a change would break and will not apply it', async () => {
		await open(manage, { mods: [] });
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({
				...noChange,
				nodes: [node()],
				conflicts: [
					{
						full_name: 'Author-Farming',
						version: '2.1.0',
						dependency: 'Author-Lib',
						requires: '1.1.0',
						have: '1.0.0',
						locked: true
					}
				]
			})
		);
		await browse();

		await click(button(/Install/));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain(
			'Author-Farming 2.1.0 needs Author-Lib 1.1.0 or newer, and this change leaves it at 1.0.0. Author-Lib is locked.'
		);
		expect(disabled(within(dialog).getByRole('button', { name: 'Install mod' }))).toBe(true);
	});

	it('shows what a modpack update removes and which of your changes it keeps', async () => {
		await open(manage, {
			mods: [
				installed({
					full_name: 'Author-Pack',
					name: 'Pack',
					is_pack: true,
					update_version: '2.0.0'
				}),
				installed({
					pack: 'Author-Pack',
					pack_version: '1.0.0',
					version: '1.5.0',
					pack_override: true
				})
			]
		});
		expect(screen.getByText('modpack')).toBeTruthy();
		expect(screen.getByText(/Part of the Pack modpack, which has 1.0.0/)).toBeTruthy();
		daemon.on('POST', `${base}/resolve`, () =>
			Response.json({
				nodes: [node({ full_name: 'Author-Pack', from_version: '1.0.0', version: '2.0.0' })],
				removals: [{ full_name: 'Author-Old', source: 'thunderstore', version: '1.0.0' }],
				kept: [
					{ full_name: 'Author-Sailing', version: '1.5.0', pack_version: '2.0.0', reason: 'manual' }
				],
				conflicts: [],
				backup: true
			})
		);

		await click(button('Update'));
		const dialog = await screen.findByRole('dialog');
		expect(text(within(dialog).getByText('Author-Old').parentElement)).toContain('removed');
		expect(text(within(dialog).getByTestId('kept-members'))).toContain(
			"Author-Sailing stays at 1.5.0, not the modpack's 2.0.0: you installed this version yourself."
		);
		expect(within(dialog).getByRole('button', { name: 'Back up and update' })).toBeTruthy();
	});

	it('locks a mod and stops offering it an update', async () => {
		await open(manage, { mods: [installed({ update_version: '1.2.0' })] });
		daemon.on('PATCH', `${base}/Author-Sailing`, () =>
			Response.json(installed({ update_version: '1.2.0', locked: true }))
		);
		daemon.on('GET', base, () =>
			Response.json({
				mods: [installed({ update_version: '1.2.0', locked: true })],
				plugin_load: boot
			})
		);

		await click(button('Lock Author-Sailing'));
		await screen.findByRole('button', { name: 'Unlock Author-Sailing' });
		expect(daemon.requests('PATCH', `${base}/Author-Sailing`)[0].body).toEqual({ locked: true });
		expect(screen.queryByRole('button', { name: 'Update' })).toBeNull();
		expect(
			screen.queryByRole('button', { name: 'Change the version of Author-Sailing' })
		).toBeNull();
	});
});
