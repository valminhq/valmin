import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RcloneSetup } from '#lib/api/remote-backups.js';
import { leave } from '#lib/nav.js';
import { envelope, FakeDaemon, job } from '#lib/testing/daemon.js';
import { click } from '#lib/testing/interact.js';
import Dialog from './rclone-connection-dialog.svelte';

vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/admin/remote-backups') } }));
vi.mock('$app/paths', () => ({
	resolve: (route: string) => (route.startsWith('/') ? route : `/${route}`)
}));
vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));
vi.mock('#lib/nav.js', async (original) => ({
	...(await original<typeof import('#lib/nav.js')>()),
	leave: vi.fn()
}));

let daemon: FakeDaemon;

function setup(overrides: Partial<RcloneSetup> = {}): RcloneSetup {
	return {
		id: 'setup-1',
		provider: 'drive',
		name: 'drive-backups',
		step: 'authorize',
		callback_url: 'http://localhost/admin/rclone-callback',
		expires_at: '2026-10-11T12:15:00Z',
		...overrides
	};
}

async function type(label: RegExp, value: string) {
	await fireEvent.input(screen.getByLabelText(label), { target: { value } });
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	vi.mocked(leave).mockClear();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('the create connection dialog', () => {
	it('shows the callback URL and sends the administrator to the provider', async () => {
		const authorization = 'https://accounts.example/auth?state=setup-1.nonce';
		daemon.on('POST', '/admin/rclone-setups', () =>
			Response.json(setup({ authorization_url: authorization }), { status: 201 })
		);
		render(Dialog, { open: true, oncreated: vi.fn() });

		expect(screen.getByText('http://localhost/admin/rclone-callback')).toBeTruthy();
		await type(/Client ID/, 'client');
		await type(/Client secret/, 'client-secret');
		await click(screen.getByRole('button', { name: 'Connect account' }));

		await vi.waitFor(() => expect(leave).toHaveBeenCalledWith(authorization));
		expect(daemon.requests('POST', '/admin/rclone-setups')[0].body).toEqual({
			provider: 'drive',
			name: 'drive-backups',
			client_id: 'client',
			client_secret: 'client-secret'
		});
	});

	it('saves an S3 connection with only the S3 fields and reports the new name', async () => {
		daemon.on('POST', '/admin/rclone-setups', () =>
			Response.json(setup({ provider: 's3', name: 's3-backups', step: 'ready' }), { status: 201 })
		);
		daemon.on('POST', '/admin/rclone-setups/setup-1/finish', () =>
			Response.json(job({ job_id: 'job-9', kind: 'remote_create' }), { status: 202 })
		);
		daemon.on('GET', '/jobs/job-9', () =>
			Response.json(job({ job_id: 'job-9', kind: 'remote_create', status: 'succeeded' }))
		);
		const oncreated = vi.fn();
		render(Dialog, { open: true, oncreated });

		await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 's3' } });
		await type(/Endpoint/, 'https://s3.example.com');
		await type(/Access key ID/, 'AKIA');
		await type(/Secret access key/, 'secret');
		await click(screen.getByRole('button', { name: 'Save connection' }));

		await vi.waitFor(() => expect(oncreated).toHaveBeenCalledWith('s3-backups'));
		expect(daemon.requests('POST', '/admin/rclone-setups')[0].body).toEqual({
			provider: 's3',
			name: 's3-backups',
			s3_provider: 'Other',
			endpoint: 'https://s3.example.com',
			region: '',
			access_key_id: 'AKIA',
			secret_access_key: 'secret',
			session_token: ''
		});
	});

	it('resumes a OneDrive setup and sends only the chosen drive', async () => {
		const drives = [{ id: 'drive-a', name: 'Personal files', type: 'personal' }];
		daemon.on('GET', '/admin/rclone-setups/setup-1', () =>
			Response.json(setup({ provider: 'onedrive', name: 'od', step: 'choose_drive', drives }))
		);
		daemon.on('POST', '/admin/rclone-setups/setup-1/advance', () =>
			Response.json(setup({ provider: 'onedrive', name: 'od', step: 'ready' }))
		);
		render(Dialog, { open: true, resumeId: 'setup-1', oncreated: vi.fn() });

		await click(await screen.findByLabelText(/Personal files/));
		await click(screen.getByRole('button', { name: 'Use this drive' }));

		await screen.findByRole('button', { name: 'Save connection' });
		expect(daemon.requests('POST', '/admin/rclone-setups/setup-1/advance')[0].body).toEqual({
			drive_id: 'drive-a'
		});
	});

	it('explains an expired setup and offers a fresh start', async () => {
		daemon.on('GET', '/admin/rclone-setups/setup-1', () =>
			envelope(404, 'not_found', 'This connection setup expired or was cancelled. Start again.')
		);
		render(Dialog, { open: true, resumeId: 'setup-1', oncreated: vi.fn() });

		await screen.findByText('This connection setup expired or was cancelled. Start again.');
		expect(screen.getByLabelText('Provider')).toBeTruthy();
	});
});
