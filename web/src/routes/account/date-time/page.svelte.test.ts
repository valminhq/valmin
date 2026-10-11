import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { viewerZone } from '$lib/api/schedules';
import type { User } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

const ada: User = {
	id: 'u-1',
	username: 'ada',
	role: 'admin',
	disabled: false,
	owner: true,
	created_at: '2026-09-01T00:00:00Z',
	last_login_at: null,
	timezone: '',
	hour_cycle: '',
	date_order: ''
};

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.user = ada;
	daemon.on('PATCH', '/me', (req) => Response.json({ ...ada, ...(req.body as Partial<User>) }));
});

afterEach(() => {
	session.user = null;
	vi.unstubAllGlobals();
});

const zoneInput = () => screen.getByLabelText('Time zone') as HTMLInputElement;
const save = () => screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement;

describe('the time zone screen', () => {
	it('saves a zone on the account, and the panel then reads times in it', async () => {
		render(Page);
		await fireEvent.input(zoneInput(), { target: { value: 'Asia/Tokyo' } });
		await click(save());

		expect(await screen.findByText('Time zone saved')).toBeTruthy();
		expect(daemon.requests('PATCH', '/me')[0].body).toEqual({ timezone: 'Asia/Tokyo' });
		expect(session.user?.timezone).toBe('Asia/Tokyo');
		expect(viewerZone()).toBe('Asia/Tokyo');
	});

	it('will not save a zone that is not in the list', async () => {
		render(Page);
		await fireEvent.input(zoneInput(), { target: { value: 'Mars/Olympus' } });

		expect(screen.getByText(/Choose a zone from the list/)).toBeTruthy();
		expect(save().disabled).toBe(true);
	});

	it('goes back to following the browser', async () => {
		session.user = { ...ada, timezone: 'Asia/Tokyo' };
		render(Page);
		await click(screen.getByRole('button', { name: 'Follow my browser' }));

		await vi.waitFor(() => expect(daemon.requests('PATCH', '/me')).toHaveLength(1));
		expect(daemon.requests('PATCH', '/me')[0].body).toEqual({ timezone: '' });
		await vi.waitFor(() => expect(session.user?.timezone).toBe(''));
		expect(viewerZone()).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
	});

	it('saves the clock and date format on the account', async () => {
		render(Page);
		await fireEvent.change(screen.getByLabelText('Clock'), { target: { value: 'h23' } });
		await fireEvent.change(screen.getByLabelText('Date'), { target: { value: 'ymd' } });
		await click(screen.getByRole('button', { name: 'Save format' }));

		expect(await screen.findByText('Format saved')).toBeTruthy();
		expect(daemon.requests('PATCH', '/me')[0].body).toEqual({
			hour_cycle: 'h23',
			date_order: 'ymd'
		});
		expect(session.user?.hour_cycle).toBe('h23');
		expect(session.user?.date_order).toBe('ymd');
	});
});
