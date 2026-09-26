import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DiagnosticsReport } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

const report: DiagnosticsReport = {
	generated_at: '2026-09-24T12:00:00Z',
	build: { version: '1.0.0', commit: 'abc', built_at: '', modified: false, go: 'go1.27.0' },
	started_at: '2026-09-24T08:00:00Z',
	checks: [
		{
			id: 'docker',
			group: 'Host',
			title: 'Container engine',
			status: 'ok',
			source: 'live',
			detail: 'Docker answered.'
		},
		{
			id: 'steam',
			group: 'Network',
			title: 'Steam reachable',
			status: 'fail',
			source: 'job',
			detail: 'The last deep check could not reach Steam.',
			remedy: 'Check the host firewall.'
		}
	],
	instances: [],
	migrations: [],
	failed_jobs: []
};

async function open(global: string[]) {
	daemon.on('GET', '/admin/diagnostics', () => Response.json(report));
	session.permissions = permissions('', [], global);
	render(Page);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

// 03 §2, ADR-193: a published port is not a reachable one and a recorded stamp is not a live
// probe; every row says which it is.
describe('the diagnostics screen', () => {
	it('is closed, and asks nothing, without panel.settings', async () => {
		await open([actions.create]);
		expect(screen.getByText('Diagnostics are not available to you.')).toBeTruthy();
		expect(daemon.requests('GET', '/admin/diagnostics')).toHaveLength(0);
	});

	it('says where every answer came from', async () => {
		await open([actions.panelSettings]);

		const engine = (await screen.findByText('Container engine')).parentElement;
		expect(text(engine)).toContain('Live check');
		const steam = screen.getByText('Steam reachable').parentElement;
		expect(text(steam)).toContain('Background check');
		expect(screen.getByText('Check the host firewall.')).toBeTruthy();
	});

	it('denies that a published port is a reachable one', async () => {
		await open([actions.panelSettings]);
		await screen.findByText('Container engine');
		expect(text(document.body)).toMatch(/This does not test reachability from the internet/);
	});

	it('filters to problems when asked', async () => {
		await open([actions.panelSettings]);
		await screen.findByText('Container engine');

		await click(screen.getByLabelText('Show problems only'));
		expect(screen.queryByText('Container engine')).toBeNull();
		expect(screen.getByText('Steam reachable')).toBeTruthy();
	});

	// The deep checks start containers, so they are a job; the bundle is a download link.
	it('runs the deep checks as a job and re-reads the report when it ends', async () => {
		await open([actions.panelSettings]);
		daemon.on('POST', '/admin/diagnostics/run', () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ status: 'succeeded' })));
		await screen.findByText('Container engine');

		expect(text(document.body)).toMatch(/throwaway container each/);
		await click(screen.getByRole('button', { name: 'Run deep checks' }));
		await vi.waitFor(() => expect(daemon.requests('GET', '/admin/diagnostics')).toHaveLength(2));
		expect(screen.getByRole('link', { name: 'Download support bundle' }).getAttribute('href')).toBe(
			'/api/v1/admin/diagnostics/bundle'
		);
	});
});
