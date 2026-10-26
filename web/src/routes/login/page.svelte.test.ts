import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { leave } from '#lib/nav.js';
import type { User } from '#lib/api/types.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import Page from './+page.svelte';

const app = vi.hoisted(() => ({ page: { url: new URL('http://localhost/login') } }));
vi.mock('$app/state', () => app);
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('#lib/nav.js', async (original) => ({
	...(await original<typeof import('#lib/nav.js')>()),
	leave: vi.fn()
}));

const ada: User = {
	id: 'user-ada',
	username: 'ada',
	role: 'admin',
	disabled: false,
	owner: true,
	created_at: '2026-01-01T00:00:00Z',
	last_login_at: null,
	timezone: '',
	hour_cycle: '',
	date_order: ''
};

let daemon: FakeDaemon;

async function signIn(search: string) {
	app.page.url = new URL('http://localhost/login' + search);
	render(Page);
	await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'ada' } });
	await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'secret' } });
	await click(screen.getByRole('button', { name: 'Sign in' }));
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('POST', '/auth/login', () => Response.json(ada));
	vi.mocked(goto).mockReset();
	vi.mocked(leave).mockReset();
});

afterEach(() => {
	session.signedOut();
	vi.unstubAllGlobals();
});

describe('signing in', () => {
	it('returns to the page that was being opened', async () => {
		vi.mocked(goto).mockResolvedValue();
		await signIn('?next=%2Finstances%2Fa%2Fmods');

		await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/instances/a/mods'));
		expect(leave).not.toHaveBeenCalled();
	});

	it('opens a page that matches no route with a full page load', async () => {
		vi.mocked(goto).mockRejectedValue(new Error('navigation_route_missing'));
		await signIn('?next=%2Ftypo');

		await vi.waitFor(() => expect(leave).toHaveBeenCalledWith('/typo'));
		expect(screen.queryByText(/could not complete/i)).toBeNull();
	});
});
