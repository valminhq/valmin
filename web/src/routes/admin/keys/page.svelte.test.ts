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
	daemon.on('POST', '/admin/keys/rotate', () => Response.json(job(), { status: 202 }));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job({ message: 'Re-encrypting secrets' })));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

// ADR-166: rotation leaves the master key alone, so what it does not do is read before the
// button is pressed, not in the result.
describe('the key rotation screen', () => {
	it('says what rotation does not remediate, above the control', () => {
		session.permissions = permissions('', [], [actions.panelSettings]);
		render(Page);

		const caveat = screen.getByText(/This does not replace the master key\./);
		const button = screen.getByRole('button', { name: 'Rotate derived keys' });
		expect(
			caveat.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING,
			'the caveat comes first'
		).toBeTruthy();
	});

	it('offers no rotation without panel.settings', () => {
		session.permissions = permissions('', [], [actions.create]);
		render(Page);
		expect(screen.queryByRole('button', { name: 'Rotate derived keys' })).toBeNull();
	});

	it('rotates as a job and holds the button while it runs', async () => {
		session.permissions = permissions('', [], [actions.panelSettings]);
		render(Page);

		const button = screen.getByRole('button', { name: 'Rotate derived keys' }) as HTMLButtonElement;
		await click(button);
		expect(await screen.findByText(/Re-encrypting secrets/)).toBeTruthy();
		expect(daemon.requests('POST', '/admin/keys/rotate')).toHaveLength(1);
		expect(button.disabled).toBe(true);
	});
});
