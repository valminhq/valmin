import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { page } from '$app/state';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, permissions } from '$lib/testing/daemon';
import Page from './+page.svelte';

vi.mock('$app/state', async () => {
	const { SvelteURL } = await import('svelte/reactivity');
	return {
		page: {
			params: { id: 'inst-a' },
			url: new SvelteURL('http://localhost/instances/inst-a/configs')
		}
	};
});

let daemon: FakeDaemon;

const listed = [
	{
		file: 'Author.Sailing.cfg',
		plugin: 'Sailing Overhaul',
		size_bytes: 120,
		installed_mods: ['Author-Sailing'],
		dir: 'BepInEx/config'
	},
	{
		file: 'com.example.wards.cfg',
		plugin: 'Wards',
		size_bytes: 80,
		installed_mods: [],
		dir: 'BepInEx/config'
	}
];

function serveList(items = listed) {
	daemon.on('GET', '/instances/inst-a/configs', () => Response.json({ items }));
}

const search = () => screen.findByLabelText('Search by filename or plugin');

async function typeQuery(value: string) {
	await fireEvent.input(await search(), { target: { value } });
}

beforeEach(() => {
	page.url.search = '';
	daemon = new FakeDaemon();
	daemon.install();
	session.permissions = permissions('inst-a', [actions.configRead, actions.configEdit]);
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the mod configuration list', () => {
	it('is headed Mod configuration', async () => {
		serveList();
		render(Page);

		expect(await screen.findByRole('heading', { name: 'Mod configuration' })).toBeTruthy();
	});

	// The daemon composes the sentence because why a mod has no file yet is Valheim knowledge,
	// and this screen renders it as sent.
	it('renders the daemon’s own sentence when there is nothing to configure', async () => {
		daemon.on('GET', '/instances/inst-a/configs', () =>
			Response.json({ items: [], note: 'Sent by the daemon, word for word.' })
		);
		render(Page);

		expect(await screen.findByText('Nothing to configure yet')).toBeTruthy();
		expect(screen.getByText('Sent by the daemon, word for word.')).toBeTruthy();
		expect(screen.queryByLabelText('Search by filename or plugin')).toBeNull();
	});

	it('links each file to its editor', async () => {
		serveList([
			{
				file: 'Author.Sailing.cfg',
				plugin: 'Sailing',
				size_bytes: 120,
				installed_mods: [],
				dir: 'BepInEx/config'
			}
		]);
		render(Page);

		const link = await screen.findByRole('link', { name: /Author\.Sailing\.cfg/ });
		expect(link.getAttribute('href')).toBe('/instances/inst-a/configs/Author.Sailing.cfg');
	});

	it('offers a retry when the list could not be read', async () => {
		daemon.on('GET', '/instances/inst-a/configs', () => envelope(500, 'internal', 'Broken.'));
		render(Page);

		expect(
			await screen.findByRole('button', { name: 'Retry loading mod configuration' })
		).toBeTruthy();
		expect(screen.queryByText('Nothing to configure yet')).toBeNull();
	});
});

describe('searching the list', () => {
	it('counts every file before anything is typed', async () => {
		serveList();
		render(Page);

		expect(await screen.findByText(/2 of 2 files/)).toBeTruthy();
		expect(screen.getAllByRole('link')).toHaveLength(2);
	});

	it('narrows by filename and by plugin name, ignoring case, and counts the result', async () => {
		serveList();
		render(Page);

		await typeQuery('SAILING.C');
		expect(screen.getAllByRole('link').map((a) => a.textContent)).toEqual([
			expect.stringContaining('Author.Sailing.cfg')
		]);
		expect(screen.getByText(/1 of 2 files/)).toBeTruthy();

		await typeQuery('wards');
		expect(screen.getAllByRole('link').map((a) => a.textContent)).toEqual([
			expect.stringContaining('com.example.wards.cfg')
		]);
	});

	it('says so when no file matches', async () => {
		serveList();
		render(Page);

		await typeQuery('nothing like this');

		expect(screen.getByText('No files match')).toBeTruthy();
		expect(screen.queryByRole('link')).toBeNull();
		expect(screen.getByText(/0 of 2 files/)).toBeTruthy();
	});

	it('opens already narrowed by the q in the address', async () => {
		page.url.search = '?q=Author.Sailing.cfg';
		serveList();
		render(Page);

		expect(((await search()) as HTMLInputElement).value).toBe('Author.Sailing.cfg');
		expect(screen.getAllByRole('link')).toHaveLength(1);
		expect(screen.getByText(/1 of 2 files/)).toBeTruthy();
	});
});

describe('deleting a file', () => {
	const deletePath = (file: string) => `/instances/inst-a/configs/${file}`;

	it('marks the files no installed mod uses', async () => {
		serveList();
		render(Page);

		const unused = await screen.findByRole('link', { name: /com\.example\.wards\.cfg/ });
		expect(unused.textContent).toContain('No installed mod');
		const used = screen.getByRole('link', { name: /Author\.Sailing\.cfg/ });
		expect(used.textContent).not.toContain('No installed mod');
	});

	it('deletes a file no installed mod uses after a plain confirmation', async () => {
		serveList();
		daemon.on(
			'DELETE',
			deletePath('com.example.wards.cfg'),
			() => new Response(null, { status: 204 })
		);
		render(Page);

		await fireEvent.click(
			await screen.findByRole('button', { name: 'Delete com.example.wards.cfg' })
		);
		expect(await screen.findByText(/No installed mod uses this file/)).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Delete config file' }));

		await vi.waitFor(() =>
			expect(screen.queryByRole('link', { name: /com\.example\.wards\.cfg/ })).toBeNull()
		);
		const [sent] = daemon.requests('DELETE', deletePath('com.example.wards.cfg'));
		expect(sent.query.get('allow_installed')).toBe('false');
	});

	it('warns before deleting a file an installed mod uses', async () => {
		serveList();
		daemon.on(
			'DELETE',
			deletePath('Author.Sailing.cfg'),
			() => new Response(null, { status: 204 })
		);
		render(Page);

		await fireEvent.click(await screen.findByRole('button', { name: 'Delete Author.Sailing.cfg' }));
		expect(await screen.findByText(/Author-Sailing is installed and uses this file/)).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Delete config file' }));

		await vi.waitFor(() =>
			expect(daemon.requests('DELETE', deletePath('Author.Sailing.cfg'))).toHaveLength(1)
		);
		const [sent] = daemon.requests('DELETE', deletePath('Author.Sailing.cfg'));
		expect(sent.query.get('allow_installed')).toBe('true');
	});

	it('sends nothing when the confirmation is cancelled', async () => {
		serveList();
		render(Page);

		await fireEvent.click(
			await screen.findByRole('button', { name: 'Delete com.example.wards.cfg' })
		);
		await fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));

		expect(daemon.requests('DELETE', deletePath('com.example.wards.cfg'))).toHaveLength(0);
	});

	it('offers no delete without config.edit', async () => {
		session.permissions = permissions('inst-a', [actions.configRead]);
		serveList();
		render(Page);

		await screen.findByRole('link', { name: /com\.example\.wards\.cfg/ });
		expect(screen.queryByRole('button', { name: /^Delete / })).toBeNull();
	});
});
