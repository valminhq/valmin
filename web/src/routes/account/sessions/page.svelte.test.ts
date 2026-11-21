import { render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DeviceSession } from '#lib/api/sessions.js';
import { FakeDaemon, envelope } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import Page from './+page.svelte';

vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

let daemon: FakeDaemon;

function device(overrides: Partial<DeviceSession> = {}): DeviceSession {
	return {
		id: 's-1',
		created_at: '2026-09-20T10:00:00Z',
		last_seen_at: '2026-09-21T10:00:00Z',
		ip: '203.0.113.7',
		user_agent: 'Firefox on Linux',
		current: false,
		...overrides
	};
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('the signed-in devices screen', () => {
	it('lists every session and marks this browser, which cannot be signed out here', async () => {
		daemon.on('GET', '/me/sessions', () =>
			Response.json({
				items: [device({ id: 's-here', ip: '198.51.100.2', current: true }), device()],
				next_cursor: null
			})
		);
		render(Page);

		expect(await screen.findByText('This browser')).toBeTruthy();
		expect(screen.getAllByRole('button', { name: 'Sign out' })).toHaveLength(1);
		expect(screen.getAllByText('Firefox on Linux')).toHaveLength(2);
	});

	it('signs another device out only after the answer, then reads the list again', async () => {
		let rows = [device({ id: 's-here', current: true }), device()];
		daemon.on('GET', '/me/sessions', () => Response.json({ items: rows, next_cursor: null }));
		daemon.on('DELETE', '/me/sessions/s-1', () => {
			rows = rows.filter((row) => row.id !== 's-1');
			return new Response(null, { status: 204 });
		});
		render(Page);

		await click(await screen.findByRole('button', { name: 'Sign out' }));
		expect(daemon.requests('DELETE', '/me/sessions/s-1'), 'nothing before the answer').toHaveLength(
			0
		);

		const dialog = await screen.findByRole('dialog');
		await click(within(dialog).getByRole('button', { name: 'Sign out' }));

		await vi.waitFor(() => expect(daemon.requests('DELETE', '/me/sessions/s-1')).toHaveLength(1));
		await vi.waitFor(() => expect(screen.queryByRole('button', { name: 'Sign out' })).toBeNull());
	});

	// A refusal has to stay visible: reading the list again must not clear it.
	it('keeps the error when the sign-out is refused', async () => {
		daemon.on('GET', '/me/sessions', () =>
			Response.json({
				items: [device({ id: 's-here', current: true }), device()],
				next_cursor: null
			})
		);
		daemon.on('DELETE', '/me/sessions/s-1', () => envelope(500, 'internal', 'Broken.'));
		render(Page);

		await click(await screen.findByRole('button', { name: 'Sign out' }));
		const dialog = await screen.findByRole('dialog');
		await click(within(dialog).getByRole('button', { name: 'Sign out' }));

		expect(await screen.findByText('Broken.')).toBeTruthy();
		expect(screen.getAllByRole('button', { name: 'Sign out' })).toHaveLength(1);
	});
});
