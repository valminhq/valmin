import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto, replaceState } from '$app/navigation';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { envelope, FakeDaemon, permissions } from '$lib/testing/daemon';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({
	page: { url: new URL('http://localhost/admin/rclone-callback') }
}));
vi.mock('$app/state', () => app);
vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));
vi.mock('$app/paths', () => ({ resolve: (route: string) => route }));

const advancePath = '/admin/rclone-setups/setup-1/advance';
let daemon: FakeDaemon;

function arrive(query: string) {
	app.page.url = new URL('http://localhost/admin/rclone-callback?' + query);
	session.permissions = permissions('', [], [actions.panelSettings]);
	render(Page);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(goto).mockClear();
	vi.mocked(replaceState).mockClear();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the rclone authorization callback', () => {
	it('clears the address bar, sends the callback once and returns to the setup', async () => {
		daemon.on('POST', advancePath, () => Response.json({ id: 'setup-1', step: 'ready' }));
		arrive('code=auth-code&state=setup-1.nonce');

		expect(replaceState).toHaveBeenCalledWith('/admin/rclone-callback', {});
		await vi.waitFor(() =>
			expect(goto).toHaveBeenCalledWith('/admin/remote-backups?setup=setup-1', {
				replaceState: true
			})
		);
		expect(daemon.requests('POST', advancePath).map((r) => r.body)).toEqual([
			{ state: 'setup-1.nonce', code: 'auth-code', error: '' }
		]);
	});

	it('shows a refused authorization with a way back to the setup', async () => {
		daemon.on('POST', advancePath, () =>
			envelope(409, 'invalid_state', 'Authorization was denied at the provider.')
		);
		arrive('error=access_denied&state=setup-1.nonce');

		await screen.findByText('Authorization was denied at the provider.');
		const back = screen.getByRole('link', { name: 'Return to setup' });
		expect(back.getAttribute('href')).toBe('/admin/remote-backups?setup=setup-1');
		expect(goto).not.toHaveBeenCalled();
	});

	it('sends nothing without a provider redirect', () => {
		arrive('');
		expect(screen.getByText(/finishes a storage provider's sign-in/)).toBeTruthy();
		expect(daemon.sent).toHaveLength(0);
	});
});
