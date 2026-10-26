import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeDaemon, envelope } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import ChangelogToggle from './changelog-toggle.svelte';

let daemon: FakeDaemon;

const path = '/instances/inst-a/mods/Author-Mod/changelog';
const props = { instanceId: 'inst-a', fullName: 'Author-Mod', version: '1.1.0' } as const;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => vi.unstubAllGlobals());

describe('the changelog toggle', () => {
	it('reads the changelog of the version the first time it is opened', async () => {
		daemon.on('GET', path, () =>
			Response.json({
				version: '1.1.0',
				text: '1.1.0\n- fixed the crash',
				truncated: false,
				registry_url: 'https://thunderstore.io/c/valheim/p/Author/Mod/'
			})
		);
		render(ChangelogToggle, { ...props, source: 'thunderstore' });
		expect(daemon.requests('GET', path)).toHaveLength(0);

		await click(screen.getByText('Changelog'));

		expect(await screen.findByText(/fixed the crash/)).toBeTruthy();
		expect(daemon.requests('GET', path)[0].query.get('version')).toBe('1.1.0');
		expect(daemon.requests('GET', path)[0].query.get('source')).toBe('thunderstore');
		const link = screen.getByRole('link', { name: 'Open the package page' }) as HTMLAnchorElement;
		expect(link.href).toBe('https://thunderstore.io/c/valheim/p/Author/Mod/');
	});

	it('says a version ships no changelog rather than failing', async () => {
		daemon.on('GET', path, () => envelope(404, 'not_found', 'Not found.'));
		render(ChangelogToggle, props);
		await click(screen.getByText('Changelog'));
		expect(await screen.findByText('Version 1.1.0 ships no changelog.')).toBeTruthy();
	});

	it('says when the registry could not be reached', async () => {
		daemon.on('GET', path, () =>
			envelope(503, 'unavailable', 'The registry could not be reached.')
		);
		render(ChangelogToggle, props);
		await click(screen.getByText('Changelog'));
		expect(await screen.findByText(/could not be read from the registry/)).toBeTruthy();
	});

	it('notes a changelog that was cut', async () => {
		daemon.on('GET', path, () =>
			Response.json({ version: '1.1.0', text: 'long', truncated: true, registry_url: '' })
		);
		render(ChangelogToggle, props);
		await click(screen.getByText('Changelog'));
		expect(await screen.findByText(/longer than shown here/)).toBeTruthy();
		expect(screen.queryByRole('link')).toBeNull();
	});
});
