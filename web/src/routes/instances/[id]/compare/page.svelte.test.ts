import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { actions, type Instance } from '$lib/api/instances';
import type { InstanceManifest, ManifestConfig, ManifestLaunch } from '$lib/api/manifest';
import type { InstalledMod } from '$lib/api/mods';
import type { MyPermissions } from '$lib/api/types';
import { instanceList } from '$lib/state/instances.svelte';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance } from '$lib/testing/daemon';
import { text } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/compare') }
}));

vi.mock('$app/state', () => app);
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const full = [actions.view, actions.settings, actions.modsList, actions.configRead];

function grants(held: Record<string, string[]>): MyPermissions {
	return {
		user_id: 'u-test',
		role: 'member',
		allowed_actions: [],
		instances: Object.entries(held).map(([instance_id, allowed_actions]) => ({
			instance_id,
			allowed_actions
		}))
	};
}

function launch(overrides: Partial<ManifestLaunch> = {}): ManifestLaunch {
	return {
		server_name: 'My Server',
		world_name: 'MyWorld',
		public: false,
		crossplay: false,
		mem_limit_mb: 4096,
		cpu_limit: null,
		backup_keep_cold: 7,
		backup_keep_hot: 3,
		backup_on_restart: false,
		...overrides
	};
}

function mod(
	full_name: string,
	version = '1.0.0',
	enabled = true,
	source: InstalledMod['source'] = 'thunderstore'
): InstalledMod {
	return {
		full_name,
		source,
		namespace: '',
		name: '',
		version,
		installed_as: 'explicit',
		update_version: '',
		is_deprecated: false,
		not_indexed: false,
		side: 'unknown',
		enabled,
		installed_at: '2026-09-01T00:00:00Z',
		file_count: 1,
		config_file_count: 0,
		load_status: null,
		load_error: null
	};
}

interface Server {
	id: string;
	name: string;
	build?: string;
	launch?: ManifestLaunch;
	mods?: InstalledMod[];
	configs?: ManifestConfig[];
}

let daemon: FakeDaemon;

function serve(server: Server) {
	daemon.on('GET', `/instances/${server.id}`, () =>
		Response.json(instance({ id: server.id, name: server.name, game_build_id: server.build }))
	);
	daemon.on('GET', `/instances/${server.id}/manifest`, () =>
		Response.json({
			schema: 1,
			name: server.name,
			instance: server.launch ?? launch(),
			mods: [],
			configs: server.configs ?? []
		} satisfies InstanceManifest)
	);
	daemon.on('GET', `/instances/${server.id}/mods`, () =>
		Response.json({ mods: server.mods ?? [], plugin_load: null })
	);
}

const live: Server = { id: 'inst-a', name: 'Live' };
const test: Server = { id: 'inst-b', name: 'Test' };

async function open(
	withId: string | null,
	{
		held = grants({ 'inst-a': full, 'inst-b': full }),
		servers = [instance({ id: 'inst-a', name: 'Live' }), instance({ id: 'inst-b', name: 'Test' })]
	}: { held?: MyPermissions; servers?: Instance[] } = {}
) {
	app.page.url = new URL(
		`http://localhost/instances/inst-a/compare${withId ? `?with=${withId}` : ''}`
	);
	daemon.on('GET', '/instances', () => Response.json({ items: servers, next_cursor: null }));
	session.permissions = held;
	render(Page);
	await vi.waitFor(() => expect(daemon.requests('GET', '/instances')).toHaveLength(1));
}

/** Opens a comparison of Live against Test and waits for it to render. */
async function compare(a: Partial<Server>, b: Partial<Server>) {
	serve({ ...live, ...a });
	serve({ ...test, ...b });
	await open('inst-b');
	await screen.findByRole('heading', { name: 'Game and settings' });
}

const card = (title: string) => {
	const root = screen.getByText(title).closest<HTMLElement>('[data-slot="card"]');
	if (!root) throw new Error(`no card titled ${title}`);
	return root;
};

/** The cells of the table row headed `header`, as text. */
const row = (header: string) => {
	const tr = screen.getByRole('rowheader', { name: header }).closest('tr');
	if (!tr) throw new Error(`no row for ${header}`);
	return [...tr.cells].map((cell) => text(cell).trim());
};

const squash = (cells: string[]) => cells.map((cell) => cell.replace(/\s+/g, ''));

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(goto).mockClear();
});

afterEach(() => {
	session.permissions = null;
	instanceList.release();
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the compare screen', () => {
	it('says what it needs when this server lacks settings, mods or configuration access', async () => {
		await open('inst-b', { held: grants({ 'inst-a': [actions.view, actions.settings] }) });

		expect(
			screen.getByText('Comparing needs settings, mods and configuration access on this server.')
		).toBeTruthy();
		expect(screen.queryByLabelText('Compare with')).toBeNull();
		expect(daemon.requests('GET', '/instances/inst-b/manifest')).toHaveLength(0);
	});

	it('says what it needs when a linked server lacks that access, and loads nothing', async () => {
		await open('inst-b', {
			held: grants({ 'inst-a': full, 'inst-b': [actions.view] })
		});

		expect(
			screen.getByText(
				'Comparing needs settings, mods and configuration access on the other server too.'
			)
		).toBeTruthy();
		expect(daemon.requests('GET', '/instances/inst-a/manifest')).toHaveLength(0);
		expect(daemon.requests('GET', '/instances/inst-b/manifest')).toHaveLength(0);
	});

	it('offers only the servers the member can compare, and keeps the choice in the address', async () => {
		await open(null, {
			held: grants({
				'inst-a': full,
				'inst-b': full,
				'inst-c': [actions.view, actions.settings, actions.modsList]
			}),
			servers: [
				instance({ id: 'inst-a', name: 'Live' }),
				instance({ id: 'inst-b', name: 'Test' }),
				instance({ id: 'inst-c', name: 'Staging' })
			]
		});

		await fireEvent.pointerDown(screen.getByLabelText('Compare with'), {
			button: 0,
			pointerType: 'mouse'
		});
		const options = await screen.findAllByRole('option');
		expect(options.map((o) => o.textContent?.trim())).toEqual(['Test']);

		await fireEvent.pointerUp(options[0], { button: 0, pointerType: 'mouse' });
		expect(goto).toHaveBeenCalledTimes(1);
		const [url, how] = vi.mocked(goto).mock.calls[0];
		expect(String(url)).toBe('http://localhost/instances/inst-a/compare?with=inst-b');
		expect(how).toEqual({ replaceState: true, keepFocus: true, noScroll: true });
		expect(daemon.requests('GET', '/instances/inst-a/manifest')).toHaveLength(0);
	});

	it('leads with a different game build', async () => {
		await compare({ build: '21981590' }, { build: '22000000' });

		expect(screen.getByText('Different game builds')).toBeTruthy();
		expect(row('Game build')).toEqual(['Game build', '21981590', '22000000']);
		expect(text(document.body)).toContain(
			'Differences: 1 in game and settings, 0 in mods, 0 in configuration files.'
		);
	});

	it('lists a differing launch setting with both values', async () => {
		await compare(
			{ build: '1', launch: launch({ crossplay: true }) },
			{ build: '1', launch: launch() }
		);

		expect(screen.queryByText('Different game builds')).toBeNull();
		expect(row('Crossplay')).toEqual(['Crossplay', 'Yes', 'No']);
		expect(screen.queryByRole('rowheader', { name: 'Server name' })).toBeNull();
	});

	describe('the mods section', () => {
		const a = [
			mod('Author-Alpha'),
			mod('Author-Beta'),
			mod('Author-Gamma', '2.0.0', false),
			mod('Author-Epsilon')
		];
		const b = [
			mod('Author-Beta', '1.1.0'),
			mod('Author-Gamma', '2.0.0'),
			mod('Author-Delta'),
			mod('Author-Epsilon', '1.0.0', true, 'hexium'),
			mod('Author-Same')
		];

		it.each([
			{
				name: 'a mod only on this server',
				want: ['Author-Alpha', '1.0.0 Thunderstore', 'Not installed', 'Only on Live']
			},
			{
				name: 'a mod only on the other',
				want: ['Author-Delta', 'Not installed', '1.0.0 Thunderstore', 'Only on Test']
			},
			{
				name: 'a version difference',
				want: ['Author-Beta', '1.0.0 Thunderstore', '1.1.0 Thunderstore', 'Different version']
			},
			{
				name: 'a disabled mod',
				want: [
					'Author-Gamma',
					'2.0.0 Thunderstore disabled',
					'2.0.0 Thunderstore',
					'Enabled only on Test'
				]
			},
			{
				name: 'a registry difference',
				want: ['Author-Epsilon', '1.0.0 Thunderstore', '1.0.0 Hexium', 'Different registry']
			}
		])('shows $name', async ({ want }) => {
			await compare({ mods: [...a, mod('Author-Same')] }, { mods: b });
			expect(squash(row(want[0]))).toEqual(squash(want));
		});

		it('lists only differences, with a count of what matches', async () => {
			await compare({ mods: [...a, mod('Author-Same')] }, { mods: b });
			const mods = card('Mods');
			expect(within(mods).queryByRole('rowheader', { name: 'Author-Same' })).toBeNull();
			expect(text(mods)).toContain('1 mod matches.');
			expect(
				within(mods)
					.getAllByRole('columnheader')
					.map((th) => th.textContent?.trim())
			).toEqual(['Mod', 'Live', 'Test', 'Difference']);
		});
	});

	it('diffs a differing configuration file only when it is opened', async () => {
		await compare(
			{
				configs: [
					{ file: 'a.cfg', content: 'x = 1\ny = 1\n' },
					{ file: 'only-live.cfg', content: '' },
					{ file: 'same.cfg', content: 'z\n' }
				]
			},
			{
				configs: [
					{ file: 'a.cfg', content: 'x = 2\ny = 1\n' },
					{ file: 'same.cfg', content: 'z\n' }
				]
			}
		);
		const configs = card('Configuration files');
		expect(text(configs)).toContain('only-live.cfg Only on Live');
		expect(text(configs)).toContain('1 file matches.');
		expect(text(configs)).toContain(
			'lines marked − are from Live and lines marked + are from Test'
		);
		expect(within(configs).queryByText('same.cfg')).toBeNull();

		const details = within(configs).getByText('a.cfg').closest('details');
		if (!details) throw new Error('a.cfg is not expandable');
		expect(within(details).queryByText('x = 2')).toBeNull();

		details.open = true;
		await fireEvent(details, new Event('toggle'));
		expect(await within(details).findByText('x = 2')).toBeTruthy();
		expect(within(details).getByText('x = 1')).toBeTruthy();
	});

	it('says when every section is identical', async () => {
		const same = {
			build: '21981590',
			mods: [mod('Author-Alpha')],
			configs: [{ file: 'a.cfg', content: 'x = 1\n' }]
		};
		await compare(same, same);

		const page = text(document.body);
		expect(page).toContain(
			'The two servers match in game build, settings, mods and configuration files.'
		);
		expect(page).toContain('Identical: same game build and settings.');
		expect(page).toContain('Identical: same mods, versions, registries and enabled state.');
		expect(page).toContain('Identical: every configuration file matches.');
		expect(screen.queryByRole('table')).toBeNull();
	});

	it('loads both servers in full', async () => {
		await compare({}, {});
		for (const server of ['inst-a', 'inst-b']) {
			expect(daemon.requests('GET', `/instances/${server}`)).toHaveLength(1);
			expect(daemon.requests('GET', `/instances/${server}/manifest`)).toHaveLength(1);
			expect(daemon.requests('GET', `/instances/${server}/mods`)).toHaveLength(1);
		}
	});
});
