import { render, screen } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { leave } from '#lib/nav.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import AppHeader from './app-header.svelte';

vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('#lib/nav.js', async (original) => ({
	...(await original<typeof import('#lib/nav.js')>()),
	leave: vi.fn()
}));

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(goto).mockReset();
	vi.mocked(leave).mockReset();
	session.ended = false;
});

describe('the theme switch', () => {
	it('steps through the themes and keeps the choice', async () => {
		render(AppHeader);
		await click(screen.getByRole('button', { name: /^Theme: System/ }));
		expect(localStorage.getItem('mode-watcher-mode')).toBe('light');
		await click(screen.getByRole('button', { name: /^Theme: Light/ }));
		expect(localStorage.getItem('mode-watcher-mode')).toBe('dark');
		expect(screen.getByRole('button', { name: /^Theme: Dark/ })).toBeTruthy();
	});
});

describe('signing out', () => {
	it('opens the sign-in page when the panel owns the sign-in', async () => {
		daemon.on('POST', '/auth/logout', () => new Response(null, { status: 204 }));
		render(AppHeader);
		await click(screen.getByRole('button', { name: 'Sign out', hidden: true }));

		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/login'));
		expect(leave).not.toHaveBeenCalled();
	});

	it("opens the reverse proxy's logout page when the panel names one", async () => {
		daemon.on('POST', '/auth/logout', () =>
			Response.json({ redirect_url: 'https://auth.example.com/logout' })
		);
		render(AppHeader);
		await click(screen.getByRole('button', { name: 'Sign out', hidden: true }));

		await vi.waitFor(() => expect(leave).toHaveBeenCalledWith('https://auth.example.com/logout'));
		expect(goto).not.toHaveBeenCalled();
		expect(session.ended, 'the sign-in page would check the session again').toBe(false);
	});
});
