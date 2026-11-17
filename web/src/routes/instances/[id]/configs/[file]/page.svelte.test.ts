import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { page } from '$app/state';
import type { ConfigCopy, ConfigRevision, ConfigSchema, ConfigSetting } from '#lib/api/configs.js';
import { actions, type Instance } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, envelope, instance, permissions, textResource } from '#lib/testing/daemon.js';
import { click, text } from '#lib/testing/interact.js';
import { socket } from '#lib/testing/socket.js';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: {
		params: { id: 'inst-a', file: 'Author.Sailing.cfg' },
		url: new URL('http://localhost/instances/inst-a/configs/Author.Sailing.cfg')
	}
}));
vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

const path = '/instances/inst-a/configs/Author.Sailing.cfg';
let daemon: FakeDaemon;

function setting(overrides: Partial<ConfigSetting> = {}): ConfigSetting {
	return {
		key: 'Speed',
		type: 'Single',
		description: 'How fast the boat goes.',
		default: 1,
		current: 1,
		range: null,
		options: null,
		widget: 'number',
		step: 0.1,
		...overrides
	};
}

function schema(settings: ConfigSetting[] = [setting()]): ConfigSchema {
	return {
		file: 'Author.Sailing.cfg',
		plugin: 'Sailing',
		sections: [{ name: 'General', settings }]
	};
}

function revision(id: string, overrides: Partial<ConfigRevision> = {}): ConfigRevision {
	return {
		id,
		created_at: '2026-09-01T10:00:00Z',
		source: 'found',
		user: null,
		mod: null,
		from_version: null,
		to_version: null,
		replaced_edits: false,
		baseline: false,
		size_bytes: 20,
		...overrides
	};
}

/** Serves a file's history, newest first, and the settings of each listed version. */
function serveHistory(
	base: string,
	history: ConfigRevision[],
	versions: Record<string, ConfigCopy>
) {
	daemon.on('GET', `${base}/history`, () => Response.json({ items: history, next_cursor: null }));
	for (const [id, copy] of Object.entries(versions)) {
		daemon.on('GET', `${base}/history/${id}`, () => Response.json(copy));
	}
}

/** Renders the file for a member holding `held`, once its first read has settled. */
async function open(
	held: string[],
	{
		read = schema(),
		row = instance(),
		history = [],
		versions = {}
	}: {
		read?: ConfigSchema;
		row?: Instance;
		history?: ConfigRevision[];
		versions?: Record<string, ConfigCopy>;
	} = {}
) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', path, () => Response.json(read));
	serveHistory(path, history, versions);
	session.permissions = permissions('inst-a', held);
	render(Page);
	await screen.findByLabelText('Filter settings');
}

const button = (name: string | RegExp) => screen.getByRole('button', { name });
const disabled = (el: HTMLElement) => (el as HTMLButtonElement).disabled;
const edit = [actions.configRead, actions.configEdit];

async function typeNumber(label: string, value: string) {
	const input = screen.getByLabelText(label) as HTMLInputElement;
	input.value = value;
	await fireEvent.input(input);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the config editor', () => {
	it('keeps unsaved edits across a reconnect and re-reads only the server row', async () => {
		await open(edit);
		await typeNumber('Speed', '2.5');

		socket.reconnect();

		await vi.waitFor(() => expect(daemon.requests('GET', '/instances/inst-a')).toHaveLength(2));
		expect(daemon.requests('GET', path)).toHaveLength(1);
		expect((screen.getByLabelText('Speed') as HTMLInputElement).value).toBe('2.5');
	});

	it('re-reads the file on a reconnect when nothing is unsaved', async () => {
		await open(edit);

		socket.reconnect();

		await vi.waitFor(() => expect(daemon.requests('GET', path)).toHaveLength(2));
	});

	// The daemon chooses the control and sends it as `widget`. A setting whose declared
	// type would suggest a switch is drawn as whatever the daemon said.
	it('draws the control the daemon chose, whatever the declared type', async () => {
		await open(edit, {
			read: schema([
				setting({ key: 'Enabled', type: 'Boolean', current: 'true', widget: 'text' }),
				setting({ key: 'Fast', type: 'Single', current: true, widget: 'toggle' })
			])
		});

		expect((screen.getByLabelText('Enabled') as HTMLInputElement).type).toBe('text');
		expect(screen.getByLabelText('Fast').getAttribute('role')).toBe('switch');
	});

	// Editing is its own capability, and the raw escape hatch another one again.
	it('lets a member who can only read see the settings and change nothing', async () => {
		await open([actions.configRead]);

		expect(screen.getByText('You can read these settings but not change them.')).toBeTruthy();
		expect((screen.getByLabelText('Speed') as HTMLInputElement).disabled).toBe(true);
		expect(screen.queryByRole('button', { name: 'Raw text' })).toBeNull();
	});

	it('offers the raw editor only to a holder of config.raw', async () => {
		await open([...edit, actions.configRaw]);
		expect(button('Raw text')).toBeTruthy();
	});

	// The raw editor bypasses every type and range the form enforces, so config.edit alone
	// does not reach it.
	it('keeps the raw editor from a member who can edit the form', async () => {
		await open(edit);
		expect(screen.queryByRole('button', { name: 'Raw text' })).toBeNull();
	});

	// A running server takes the write and applies it on restart; the form says so.
	it('keeps the form editable on a running server and says when it applies', async () => {
		await open(edit, { row: instance({ state: 'running' }) });

		expect(screen.getByText(/Saved settings take effect when it restarts/)).toBeTruthy();
		expect((screen.getByLabelText('Speed') as HTMLInputElement).disabled).toBe(false);
	});

	// B11 / C19: the daemon refuses a write to a server in transition; the form says so up front.
	it('disables the form on a starting server and says why', async () => {
		await open(edit, { row: instance({ state: 'starting' }) });

		expect(screen.getByTestId('config-actions-blocked').textContent).toContain(
			'This server is starting.'
		);
		expect((screen.getByLabelText('Speed') as HTMLInputElement).disabled).toBe(true);
	});

	// Nothing writes except the diff dialog's confirm, and what the file holds afterwards is
	// read back from the daemon (F4).
	it('writes only the changed settings, after the diff is confirmed, then re-reads', async () => {
		await open(edit, {
			read: schema([setting(), setting({ key: 'Turn', current: 3 })])
		});
		daemon.on('PATCH', path, () => Response.json(schema()));

		await typeNumber('Speed', '2.5');
		expect(screen.getByText(/1\s+setting changed/)).toBeTruthy();
		await click(button('Review changes'));

		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('Save 1 change?');
		expect(text(dialog)).toContain('General.Speed 1 2.5');
		expect(daemon.requests('PATCH', path), 'nothing written while reviewing').toHaveLength(0);

		await click(within(dialog).getByRole('button', { name: 'Save changes' }));
		await vi.waitFor(() => expect(daemon.requests('PATCH', path)).toHaveLength(1));
		expect(daemon.requests('PATCH', path)[0].body).toEqual({ 'General.Speed': 2.5 });
		await vi.waitFor(() => expect(daemon.requests('GET', path)).toHaveLength(2));
	});

	it('writes nothing when the review is cancelled', async () => {
		await open(edit);
		await typeNumber('Speed', '2.5');
		await click(button('Review changes'));
		await click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
		expect(daemon.requests('PATCH', path)).toHaveLength(0);
	});

	// Every rejected setting is marked where it is, not as one blob at the top.
	it('renders a rejected setting beside the setting', async () => {
		await open(edit);
		daemon.on('PATCH', path, () =>
			envelope(422, 'validation_failed', 'Some settings are invalid.', [
				{ field: 'General.Speed', code: 'out_of_range', message: 'Must be at most 2.' }
			])
		);

		await typeNumber('Speed', '9');
		await click(button('Review changes'));
		await click(
			within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save changes' })
		);

		await screen.findByText(/Nothing was written/);
		const described = (screen.getByLabelText('Speed').getAttribute('aria-describedby') ?? '')
			.split(' ')
			.map((id) => document.getElementById(id)?.textContent ?? '');
		expect(described, 'the message is tied to the setting it is about').toContain(
			'Must be at most 2.'
		);
	});

	// The compared versions come from the daemon, and restoring one only fills the form: it
	// goes through the same confirmation as anything typed by hand.
	it('compares with the baseline and restores it into the form without writing', async () => {
		await open(edit, {
			read: schema([setting({ current: 2 })]),
			history: [
				revision('edit', { source: 'edit', user: 'den' }),
				revision('base', { baseline: true })
			],
			versions: {
				base: { ...schema([setting({ current: 1 })]), captured_at: '2026-09-01T10:00:00Z' }
			}
		});

		expect((screen.getByLabelText('Compare with') as HTMLSelectElement).value).toBe('base');
		expect(await screen.findByText(/1 setting differs from As first found/)).toBeTruthy();
		await click(button(/Load 1 setting from As first found/));
		expect((screen.getByLabelText('Speed') as HTMLInputElement).value).toBe('1');
		expect(daemon.requests('PATCH', path), 'restoring writes nothing').toHaveLength(0);
		expect(button('Review changes')).toBeTruthy();
	});

	it('compares with nothing, and shows no error, for a file without history', async () => {
		await open(edit);
		expect((screen.getByLabelText('Compare with') as HTMLSelectElement).value).toBe('off');
		expect(screen.queryByRole('alert')).toBeNull();
	});

	it("loads the mod's declared defaults", async () => {
		await open(edit, { read: schema([setting({ current: 2, default: 1 })]) });
		const select = screen.getByLabelText('Compare with') as HTMLSelectElement;
		select.value = 'defaults';
		await fireEvent.change(select);
		await click(
			await screen.findByRole('button', { name: /Load 1 setting from the mod's defaults/ })
		);
		expect((screen.getByLabelText('Speed') as HTMLInputElement).value).toBe('1');
	});

	it('keeps comparing with nothing after a save', async () => {
		await open(edit, { history: [revision('base', { baseline: true })] });
		const select = screen.getByLabelText('Compare with') as HTMLSelectElement;
		select.value = 'off';
		await fireEvent.change(select);
		daemon.on('PATCH', path, () => Response.json(schema([setting({ current: 2 })])));

		await typeNumber('Speed', '2');
		await click(button('Review changes'));
		await click(await screen.findByRole('button', { name: 'Save changes' }));
		await vi.waitFor(() => expect(daemon.requests('PATCH', path)).toHaveLength(1));
		await vi.waitFor(() => expect(daemon.requests('GET', `${path}/history`)).toHaveLength(2));
		expect((screen.getByLabelText('Compare with') as HTMLSelectElement).value).toBe('off');
	});

	it('leaves settings without a declared default out of the defaults', async () => {
		await open(edit, {
			read: schema([
				setting({ current: 2, default: 1 }),
				setting({ key: 'Name', type: '', widget: 'text', default: '', current: 'boat' })
			])
		});
		const select = screen.getByLabelText('Compare with') as HTMLSelectElement;
		select.value = 'defaults';
		await fireEvent.change(select);
		expect(
			await screen.findByRole('button', { name: /Load 1 setting from the mod's defaults/ })
		).toBeTruthy();
	});

	// A mod update that replaced edits says so and compares with the version it replaced.
	it('names the mod update that replaced edits and compares with what it replaced', async () => {
		await open(edit, {
			read: schema([setting({ current: 1 })]),
			history: [
				revision('mod', {
					source: 'mod',
					mod: 'Ns-Sailing',
					from_version: '1.0.0',
					to_version: '2.0.0',
					replaced_edits: true,
					baseline: true
				}),
				revision('mine', { source: 'edit', user: 'den' })
			],
			versions: {
				mine: { ...schema([setting({ current: 3 })]), captured_at: '2026-09-01T10:00:00Z' }
			}
		});

		expect(text(screen.getByTestId('config-replaced'))).toMatch(
			/Ns-Sailing 1.0.0 → 2.0.0 replaced/
		);
		expect((screen.getByLabelText('Compare with') as HTMLSelectElement).value).toBe('mine');
		expect(text(screen.getByTestId('config-predates'))).toMatch(
			/from before Ns-Sailing 1.0.0 → 2.0.0/
		);
		expect(await screen.findByText(/1 setting differs from Saved by den/)).toBeTruthy();
	});
});

describe('the raw config editor', () => {
	const raw = [...edit, actions.configRaw];

	async function openRaw(disk = 'Speed = 1\n', etag = '"v1"', history: ConfigRevision[] = []) {
		daemon.on('GET', `${path}/raw`, () => textResource(disk, etag));
		await open(raw, { history });
		await click(button('Raw text'));
		return (await screen.findByLabelText('Author.Sailing.cfg as text')) as HTMLTextAreaElement;
	}

	// A full replacement without If-Match silently discards another writer's save.
	it('saves with the ETag it read', async () => {
		const area = await openRaw();
		daemon.on('PUT', `${path}/raw`, () => textResource('Speed = 2\n', '"v2"'));

		await fireEvent.input(area, { target: { value: 'Speed = 2\n' } });
		await click(button('Save file'));

		await vi.waitFor(() => expect(daemon.requests('PUT', `${path}/raw`)).toHaveLength(1));
		const sent = daemon.requests('PUT', `${path}/raw`)[0];
		expect(sent.headers['If-Match']).toBe('"v1"');
		expect(sent.body).toBe('Speed = 2\n');
	});

	it('cannot save before it holds an ETag', async () => {
		const area = await openRaw('Speed = 1\n', '');
		await fireEvent.input(area, { target: { value: 'Speed = 2\n' } });
		expect(disabled(button('Save file'))).toBe(true);
	});

	// A refused save is shown and the operator decides. Nothing is merged, and nothing
	// is written again on its own.
	it('shows the other version after a stale save and writes nothing more by itself', async () => {
		const area = await openRaw();
		daemon.on('PUT', `${path}/raw`, () =>
			envelope(412, 'stale_write', 'The file changed since you opened it.')
		);

		await fireEvent.input(area, { target: { value: 'Speed = 2\n' } });
		daemon.on('GET', `${path}/raw`, () => textResource('Speed = 7\n', '"theirs"'));
		await click(button('Save file'));

		expect(await screen.findByText('Nothing was saved')).toBeTruthy();
		expect(
			(screen.getByLabelText('The file as it is on disk now') as HTMLTextAreaElement).value
		).toBe('Speed = 7\n');
		expect(area.value, 'my edit is still in the editor').toBe('Speed = 2\n');
		expect(daemon.requests('PUT', `${path}/raw`), 'no retry on its own').toHaveLength(1);

		daemon.on('PUT', `${path}/raw`, () => textResource('Speed = 2\n', '"v3"'));
		await click(button('Overwrite saved file'));
		await vi.waitFor(() => expect(daemon.requests('PUT', `${path}/raw`)).toHaveLength(2));
		expect(daemon.requests('PUT', `${path}/raw`)[1].headers['If-Match']).toBe('"theirs"');
	});

	// The line view shows what the by-setting comparison cannot, and diffs the editor rather
	// than the file, so an unsaved edit shows as what it will be.
	it('diffs the kept version against what is in the editor', async () => {
		daemon.on('GET', `${path}/history/base/raw`, () => textResource('Speed = 1\n', '"orig"'));
		const area = await openRaw('Speed = 1\n', '"v1"', [revision('base', { baseline: true })]);
		expect(await screen.findByText('Identical to the version being compared.')).toBeTruthy();

		await fireEvent.input(area, { target: { value: 'Speed = 1\n# tuned by hand\n' } });
		expect(await screen.findByText('1 line differs from the version being compared.')).toBeTruthy();
	});

	it('is unavailable while the form holds unsaved changes', async () => {
		await open(raw);
		await typeNumber('Speed', '2.5');
		expect(disabled(button('Raw text'))).toBe(true);
		expect(
			screen.getByText('Save or discard your changes to edit this file as text.')
		).toBeTruthy();
	});
});

describe('a config file with no settings', () => {
	const name = 'expand_world/expand_biomes.yaml';
	const filePath = `/instances/inst-a/configs/${encodeURIComponent(name)}`;
	const params = page.params as Record<string, string>;

	beforeEach(() => {
		params.file = name;
	});

	afterEach(() => {
		params.file = 'Author.Sailing.cfg';
	});

	function serve() {
		daemon.on('GET', '/instances/inst-a', () => Response.json(instance()));
		daemon.on('GET', `${filePath}/raw`, () => textResource('biomes: []\n', '"v1"'));
		serveHistory(filePath, [revision('base', { baseline: true })], {});
		daemon.on('GET', `${filePath}/history/base/raw`, () => textResource('biomes: [old]\n', '"o"'));
	}

	// The settings read would project YAML lines as settings; the daemon refuses it anyway.
	it('opens straight in the raw editor and never asks for settings', async () => {
		serve();
		session.permissions = permissions('inst-a', [...edit, actions.configRaw]);
		render(Page);

		const area = (await screen.findByLabelText(`${name} as text`)) as HTMLTextAreaElement;
		await vi.waitFor(() => expect(area.value).toBe('biomes: []\n'));
		expect(screen.queryByRole('button', { name: 'Raw text' })).toBeNull();
		expect(screen.queryByLabelText('Filter settings')).toBeNull();
		expect(daemon.requests('GET', filePath)).toHaveLength(0);
		expect(await screen.findByText(/line differs|lines differ/)).toBeTruthy();
	});

	it('says why a member without raw access cannot open it', async () => {
		serve();
		session.permissions = permissions('inst-a', edit);
		render(Page);

		expect(await screen.findByText(/no settings form/)).toBeTruthy();
		expect(screen.queryByLabelText(`${name} as text`)).toBeNull();
	});
});
