import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { User } from '$lib/api/types';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, envelope } from '$lib/testing/daemon';
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
	last_login_at: null
};

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.user = ada;
});

afterEach(() => {
	session.user = null;
	vi.unstubAllGlobals();
});

async function fill(current: string, next: string, repeat: string) {
	await fireEvent.input(screen.getByLabelText('Current password'), { target: { value: current } });
	await fireEvent.input(screen.getByLabelText('New password'), { target: { value: next } });
	await fireEvent.input(screen.getByLabelText('Confirm new password'), {
		target: { value: repeat }
	});
}

const submit = () => screen.getByRole('button', { name: 'Change password' }) as HTMLButtonElement;

describe('the change password screen', () => {
	it('sends both passwords, confirms, clears the form and stays signed in', async () => {
		daemon.on('POST', '/me/password', () => new Response(null, { status: 204 }));
		render(Page);

		await fill('old-password', 'a-brand-new-password', 'a-brand-new-password');
		await click(submit());

		expect(await screen.findByText('Password changed')).toBeTruthy();
		const [sent] = daemon.requests('POST', '/me/password');
		expect(sent.body).toEqual({
			current_password: 'old-password',
			new_password: 'a-brand-new-password'
		});
		expect((screen.getByLabelText('Current password') as HTMLInputElement).value).toBe('');
		expect((screen.getByLabelText('New password') as HTMLInputElement).value).toBe('');
		expect(daemon.requests('POST', '/auth/logout')).toHaveLength(0);
		expect(session.user?.username).toBe('ada');
	});

	it('holds the submit until the confirmation matches', async () => {
		render(Page);
		expect(submit().disabled).toBe(true);

		await fill('old-password', 'a-brand-new-password', 'a-brand-new-passwor');
		expect(screen.getByText('The passwords do not match.')).toBeTruthy();
		expect(submit().disabled).toBe(true);

		await fireEvent.input(screen.getByLabelText('Confirm new password'), {
			target: { value: 'a-brand-new-password' }
		});
		expect(screen.queryByText('The passwords do not match.')).toBeNull();
		expect(submit().disabled).toBe(false);
		expect(daemon.requests('POST', '/me/password')).toHaveLength(0);
	});

	it('reports a wrong current password and empties that field for another try', async () => {
		daemon.on('POST', '/me/password', () =>
			envelope(401, 'invalid_credentials', 'The current password is incorrect.')
		);
		render(Page);

		await fill('not-it', 'a-brand-new-password', 'a-brand-new-password');
		await click(submit());

		expect(await screen.findByText('The current password is incorrect.')).toBeTruthy();
		expect(screen.queryByText('Password changed')).toBeNull();
		expect((screen.getByLabelText('Current password') as HTMLInputElement).value).toBe('');
		expect((screen.getByLabelText('New password') as HTMLInputElement).value).toBe(
			'a-brand-new-password'
		);
	});

	it('names the field a validation failure rejected', async () => {
		daemon.on('POST', '/me/password', () =>
			envelope(422, 'validation_failed', 'Some values are invalid.', [
				{
					field: 'new_password',
					code: 'too_short',
					message: 'Password must be at least 8 characters.'
				}
			])
		);
		render(Page);

		await fill('old-password', 'short', 'short');
		await click(submit());

		expect(await screen.findByText(/Password must be at least 8 characters\./)).toBeTruthy();
		expect(screen.queryByText('Password changed')).toBeNull();
	});
});
