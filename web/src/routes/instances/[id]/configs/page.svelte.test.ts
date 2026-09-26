import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope, permissions } from '$lib/testing/daemon';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/configs') }
}));

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.permissions = permissions('inst-a', [actions.configRead, actions.configEdit]);
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the settings files list', () => {
	// ADR-110: why a mod has no file yet is Valheim knowledge, so the daemon composes the
	// sentence and this screen renders it as sent.
	it('renders the daemon’s own sentence when there is nothing to configure', async () => {
		daemon.on('GET', '/instances/inst-a/configs', () =>
			Response.json({ items: [], note: 'Sent by the daemon, word for word.' })
		);
		render(Page);

		expect(await screen.findByText('Nothing to configure yet')).toBeTruthy();
		expect(screen.getByText('Sent by the daemon, word for word.')).toBeTruthy();
	});

	it('links each file to its editor', async () => {
		daemon.on('GET', '/instances/inst-a/configs', () =>
			Response.json({
				items: [{ file: 'Author.Sailing.cfg', plugin: 'Sailing', size_bytes: 120 }]
			})
		);
		render(Page);

		const link = await screen.findByRole('link', { name: /Author\.Sailing\.cfg/ });
		expect(link.getAttribute('href')).toBe('/instances/inst-a/configs/Author.Sailing.cfg');
	});

	it('offers a retry when the list could not be read', async () => {
		daemon.on('GET', '/instances/inst-a/configs', () => envelope(500, 'internal', 'Broken.'));
		render(Page);

		expect(
			await screen.findByRole('button', { name: 'Retry loading settings files' })
		).toBeTruthy();
		expect(screen.queryByText('Nothing to configure yet')).toBeNull();
	});
});
