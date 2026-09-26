import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance, type UpdateStatus } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import UpdateNotice from './update-notice.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

function status(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
	return {
		installed_build_id: '21981590',
		public_build_id: '22110001',
		observed_at: '2026-09-24T08:00:00Z',
		update_available: true,
		...overrides
	};
}

async function open(held: string[], answer: UpdateStatus, row: Instance = instance()) {
	daemon.on('GET', '/instances/inst-a/update-status', () => Response.json(answer));
	session.permissions = permissions('inst-a', held);
	render(UpdateNotice, { instance: row });
	await vi.waitFor(() =>
		expect(daemon.requests('GET', '/instances/inst-a/update-status')).toHaveLength(1)
	);
}

const updates = () => daemon.requests('POST', '/instances/inst-a/update');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('POST', '/instances/inst-a/update', () => Response.json(job(), { status: 202 }));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the update notice', () => {
	// 12 §2.5: an available update is a property of the server, stated beside it.
	it('names both builds when a newer one is available', async () => {
		await open([actions.view], status());

		expect(await screen.findByText('A newer game build is available')).toBeTruthy();
		expect(text(screen.getByRole('alert'))).toContain('This server runs build 21981590');
		expect(text(screen.getByRole('alert'))).toContain('the public branch is on 22110001');
	});

	// Being on the current build is a fact about the server, not something to decide.
	it('states the current build quietly when nothing is outstanding', async () => {
		await open([actions.view, actions.gameUpdate], status({ update_available: false }));

		expect(await screen.findByText(/is the current public build\./)).toBeTruthy();
		expect(screen.queryByRole('alert')).toBeNull();
		expect(screen.getByRole('button', { name: 'Reinstall' })).toBeTruthy();
	});

	it('says no check has run rather than that the server is current', async () => {
		await open([actions.view], status({ update_available: null }));
		expect(await screen.findByText(/No update check has completed yet\./)).toBeTruthy();
	});

	it('shows nothing to a member who cannot update a server that is current', async () => {
		await open([actions.view], status({ update_available: false }));
		await vi.waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
		expect(screen.queryByText(/current public build/)).toBeNull();
		expect(screen.queryByRole('button')).toBeNull();
	});

	// 03 §8: a new build can break every mod, so a modded server is told and left to its
	// operator — and the update tells the daemon the operator answered for the mods.
	it('warns a modded server and sends the operator’s answer for its mods', async () => {
		await open([actions.view, actions.gameUpdate], status(), instance({ modded: true }));

		expect(await screen.findByTestId('modded-notice')).toBeTruthy();
		expect(text(screen.getByTestId('modded-notice'))).toContain(
			'Nothing updates this server on its own — a schedule skips it and records the skip.'
		);
		await click(screen.getByRole('button', { name: 'Update the game' }));
		const dialog = await screen.findByRole('dialog');
		await fireEvent.input(within(dialog).getByLabelText(/to confirm/), {
			target: { value: 'inst-a' }
		});
		await click(within(dialog).getByRole('button', { name: 'Update' }));

		await vi.waitFor(() => expect(updates()).toHaveLength(1));
		expect(updates()[0].body).toEqual({ confirm_modded: true });
	});

	// F3, 09 §3.3: instance.update is never grantable, and the control renders from it.
	it('offers the update only to a holder of instance.update', async () => {
		await open([actions.view, actions.settings], status());
		await screen.findByText('A newer game build is available');
		expect(screen.queryByRole('button', { name: 'Update the game' })).toBeNull();
	});

	// F5 and B7: the update replaces the server tree, so it is confirmed, needs a stopped
	// server, and leaves the server stopped afterwards.
	it('refuses a running server, and says why', async () => {
		await open([actions.view, actions.gameUpdate], status(), instance({ state: 'running' }));

		const button = (await screen.findByRole('button', {
			name: 'Update the game'
		})) as HTMLButtonElement;
		expect(button.disabled).toBe(true);
		expect(screen.getByText('Stop this server to update it.')).toBeTruthy();
	});

	it('sends nothing until the confirmation that says the server stays stopped', async () => {
		await open([actions.view, actions.gameUpdate], status());

		await click(await screen.findByRole('button', { name: 'Update the game' }));
		const dialog = await screen.findByRole('dialog');
		expect(text(dialog)).toContain('This server stays stopped afterwards');
		expect(text(dialog)).toContain('build 22110001');
		await click(within(dialog).getByRole('button', { name: 'Cancel' }));
		expect(updates()).toHaveLength(0);
	});
});
