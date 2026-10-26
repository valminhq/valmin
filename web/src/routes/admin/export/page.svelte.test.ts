import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, job, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('POST', '/admin/export', () => Response.json(job(), { status: 202 }));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job({ message: 'writing the full export' })));
	daemon.on('GET', '/admin/export', () => Response.json({ latest: null }));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the installation export screen', () => {
	it('offers no export without panel.settings', () => {
		session.permissions = permissions('', [], [actions.create]);
		render(Page);
		expect(screen.queryByRole('button', { name: 'Export' })).toBeNull();
	});

	it('exports in the chosen mode as a job', async () => {
		session.permissions = permissions('', [], [actions.panelSettings]);
		render(Page);

		await click(screen.getByRole('radio', { name: /Full/ }));
		const button = screen.getByRole('button', { name: 'Export' }) as HTMLButtonElement;
		await click(button);

		expect(await screen.findByText(/writing the full export/)).toBeTruthy();
		expect(daemon.requests('POST', '/admin/export').map((r) => r.body)).toEqual([{ mode: 'full' }]);
		expect(button.disabled).toBe(true);
	});

	it('offers the latest export for download', async () => {
		daemon.on('GET', '/admin/export', () =>
			Response.json({
				latest: {
					filename: 'valmin-export-light-20261025T120000Z.tar.gz',
					mode: 'light',
					size_bytes: 31457280,
					created_at: '2026-10-25T12:00:00Z'
				}
			})
		);
		session.permissions = permissions('', [], [actions.panelSettings]);
		render(Page);

		const link = await screen.findByRole('link', { name: /Download/ });
		expect(link.getAttribute('href')).toBe('/api/v1/admin/export/download');
		expect(screen.getByText(/30 MB/)).toBeTruthy();
	});
});
