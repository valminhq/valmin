import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { click } from '$lib/testing/interact';
import { FakeDaemon, envelope, instance, job, permissions } from '$lib/testing/daemon';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/players') }
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

let daemon: FakeDaemon;

/** A player list as the daemon serves it: the ids and the ETag guarding a replacement. */
function list(ids: string[], etag: string): Response {
	return Response.json({ ids }, { headers: { ETag: etag } });
}

async function open(
	held: string[],
	{ admins = ['76561198000000001'], state = 'running' }: { admins?: string[]; state?: string } = {}
) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(instance({ state })));
	daemon.on('GET', '/instances/inst-a/admins', () => list(admins, '"a1"'));
	daemon.on('GET', '/instances/inst-a/bans', () => list([], '"b1"'));
	daemon.on('GET', '/instances/inst-a/permitted', () => list([], '"p1"'));
	daemon.on('GET', '/instances/inst-a/players/seen', () =>
		Response.json({
			items: [
				{
					platform_id: 'Steam_76561198000000007',
					name: 'Kari',
					first_seen_at: '2026-09-20T10:00:00Z',
					last_seen_at: '2026-09-20T11:00:00Z'
				}
			],
			next_cursor: null
		})
	);
	session.permissions = permissions('inst-a', held);
	render(Page);
	await screen.findByText('Player access');
}

/** The card for one of the three lists, found by its title. */
async function card(title: string) {
	const heading = await screen.findByText(title, { selector: '[data-slot="card-title"]' });
	const root = heading.closest<HTMLElement>('[data-slot="card"]');
	if (!root) throw new Error(`no card titled ${title}`);
	await within(root).findByLabelText('Player IDs');
	return root;
}

const puts = () => daemon.requests('PUT', '/instances/inst-a/admins');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the player-list screen', () => {
	it('shows the three lists to a holder of players.manage', async () => {
		await open([actions.view, actions.playersManage]);
		for (const title of ['Admins', 'Banned', 'Permitted']) await card(title);
		const admins = within(await card('Admins')).getByLabelText('Player IDs') as HTMLTextAreaElement;
		expect(admins.value).toBe('76561198000000001');
	});

	it('shows no list to a member without players.manage', async () => {
		await open([actions.view]);
		expect(
			await screen.findByText('You cannot read or change this server’s player lists.')
		).toBeTruthy();
		expect(screen.queryByLabelText('Player IDs')).toBeNull();
		expect(daemon.requests('GET', '/instances/inst-a/admins')).toHaveLength(0);
	});

	// G1, 11 §1.1: a replacement says which version it started from, and the ids go back
	// exactly as entered — no platform invented, no reordering.
	it('saves the ids as entered, with the ETag they were loaded under', async () => {
		await open([actions.view, actions.playersManage]);
		daemon.on('PUT', '/instances/inst-a/admins', () =>
			list(['Steam_2', '76561198000000001'], '"a2"')
		);
		const admins = await card('Admins');

		await fireEvent.input(within(admins).getByLabelText('Player IDs'), {
			target: { value: 'Steam_2\n76561198000000001' }
		});
		await click(within(admins).getByRole('button', { name: 'Save list' }));

		await vi.waitFor(() => expect(puts()).toHaveLength(1));
		expect(puts()[0].headers['If-Match']).toBe('"a1"');
		expect(puts()[0].body).toEqual({ ids: ['Steam_2', '76561198000000001'] });
	});

	// A refused save is a decision for the operator: their edits stay, the other version is
	// shown, and nothing is written again until they choose.
	it('keeps local edits and shows the saved version after a stale write', async () => {
		await open([actions.view, actions.playersManage]);
		daemon.on('PUT', '/instances/inst-a/admins', () =>
			envelope(412, 'stale_write', 'The list changed since you opened it.')
		);
		const admins = await card('Admins');
		const field = within(admins).getByLabelText('Player IDs') as HTMLTextAreaElement;

		await fireEvent.input(field, { target: { value: 'mine' } });
		daemon.on('GET', '/instances/inst-a/admins', () => list(['theirs'], '"a9"'));
		await click(within(admins).getByRole('button', { name: 'Save list' }));

		const alert = await within(admins).findByText('This list changed elsewhere');
		expect(alert).toBeTruthy();
		expect(field.value, 'the edit is still in the field').toBe('mine');
		expect(within(admins).getByText('theirs')).toBeTruthy();
		expect(puts(), 'no second write on its own').toHaveLength(1);

		daemon.on('PUT', '/instances/inst-a/admins', () => list(['mine'], '"a10"'));
		await click(within(admins).getByRole('button', { name: 'Overwrite saved list' }));
		await vi.waitFor(() => expect(puts()).toHaveLength(2));
		expect(puts()[1].headers['If-Match']).toBe('"a9"');
	});

	it('discards local edits for the saved version when asked', async () => {
		await open([actions.view, actions.playersManage]);
		daemon.on('PUT', '/instances/inst-a/admins', () =>
			envelope(412, 'stale_write', 'The list changed since you opened it.')
		);
		const admins = await card('Admins');
		const field = within(admins).getByLabelText('Player IDs') as HTMLTextAreaElement;

		await fireEvent.input(field, { target: { value: 'mine' } });
		daemon.on('GET', '/instances/inst-a/admins', () => list(['theirs'], '"a9"'));
		await click(within(admins).getByRole('button', { name: 'Save list' }));
		await click(await within(admins).findByRole('button', { name: 'Discard my edits' }));

		expect(field.value).toBe('theirs');
		expect(puts()).toHaveLength(1);
	});

	// 11 §2.4: a rejected id is reported against its line.
	it('names the line of a rejected id', async () => {
		await open([actions.view, actions.playersManage]);
		daemon.on('PUT', '/instances/inst-a/admins', () =>
			envelope(422, 'validation_failed', 'Some ids are invalid.', [
				{ field: 'ids.1', code: 'invalid', message: 'Not a player id.' }
			])
		);
		const admins = await card('Admins');
		const field = within(admins).getByLabelText('Player IDs');

		await fireEvent.input(field, { target: { value: '76561198000000001\nnonsense' } });
		await click(within(admins).getByRole('button', { name: 'Save list' }));

		expect(await within(admins).findByText('Line 2: Not a player id.')).toBeTruthy();
		expect(field.getAttribute('aria-invalid')).toBe('true');
	});

	// Saving a list does not restart the server; restarting is the ordinary restart job,
	// behind its own action and its own confirmation.
	it('never restarts on save, and restarts only through the confirmed restart job', async () => {
		await open([actions.view, actions.playersManage, actions.restart]);
		daemon.on('PUT', '/instances/inst-a/admins', () => list(['x'], '"a2"'));
		daemon.on('POST', '/instances/inst-a/restart', () => Response.json(job(), { status: 202 }));
		daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
		const admins = await card('Admins');

		expect(screen.getByText(/Saving a list does not restart the server\./)).toBeTruthy();
		await fireEvent.input(within(admins).getByLabelText('Player IDs'), {
			target: { value: 'x' }
		});
		await click(within(admins).getByRole('button', { name: 'Save list' }));
		await vi.waitFor(() => expect(puts()).toHaveLength(1));
		expect(daemon.requests('POST', '/instances/inst-a/restart')).toHaveLength(0);

		await click(screen.getByRole('button', { name: 'Restart server' }));
		const dialog = await screen.findByRole('dialog');
		await click(within(dialog).getByRole('button', { name: 'Restart' }));
		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/restart')).toHaveLength(1)
		);
	});

	it('offers no restart without the restart action', async () => {
		await open([actions.view, actions.playersManage]);
		await card('Admins');
		expect(screen.queryByRole('button', { name: 'Restart server' })).toBeNull();
	});
});

// The sightings are evidence about people, and most of them come from a connection attempt
// that may have been refused. The screen must not promise the account played, and must show
// the id exactly as the daemon sent it: rewriting one form into the other strips an admin of
// admin (Q30).
describe('the seen-players list', () => {
	it('says what seen means, and shows the id exactly as sent', async () => {
		await open([actions.view, actions.playersManage]);

		expect(await screen.findByText('Steam_76561198000000007')).toBeTruthy();
		expect(screen.getByText(/not that it played or was let in/)).toBeTruthy();
		expect(screen.getByRole('button', { name: 'Copy Steam_76561198000000007' })).toBeTruthy();
	});
});
