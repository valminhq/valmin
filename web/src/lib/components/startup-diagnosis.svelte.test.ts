import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Finding } from '$lib/api/instances';
import { FakeDaemon } from '$lib/testing/daemon';
import StartupDiagnosis from './startup-diagnosis.svelte';

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => vi.unstubAllGlobals());

function answer(findings: Finding[]) {
	daemon.on('GET', '/instances/inst-a/diagnosis', () =>
		Response.json({ findings, checked_at: '2026-10-11T00:00:00Z' })
	);
}

describe('the start diagnosis', () => {
	it('renders each finding with its proof, its fixes, its cascade and a link to the fix', async () => {
		answer([
			{
				code: 'plugin_failed',
				severity: 'error',
				title: 'Example-Thing did not load',
				detail: 'It is not active on this server.',
				fixes: ['Update it.', 'Disable it.'],
				evidence: 'Error loading [Thing 1.0]',
				related: ['Downstream 2.0'],
				target: 'mods'
			}
		]);
		render(StartupDiagnosis, { instanceId: 'inst-a', serverState: 'error' });

		expect(await screen.findByText('Example-Thing did not load')).toBeTruthy();
		expect(screen.getByText('Error loading [Thing 1.0]')).toBeTruthy();
		expect(screen.getAllByRole('listitem').map((li) => li.textContent?.trim())).toEqual([
			'Update it.',
			'Disable it.',
			'Downstream 2.0'
		]);
		expect(screen.getByText('1 more skipped because of this')).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Open mods' }).getAttribute('href')).toBe(
			'/instances/inst-a/mods'
		);
	});

	it('renders nothing for a server with nothing to explain', async () => {
		answer([]);
		const { container } = render(StartupDiagnosis, {
			instanceId: 'inst-a',
			serverState: 'running'
		});

		await vi.waitFor(() =>
			expect(daemon.requests('GET', '/instances/inst-a/diagnosis')).toHaveLength(1)
		);
		expect(container.querySelector('section')).toBeNull();
	});
});
