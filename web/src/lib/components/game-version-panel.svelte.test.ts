import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { FakeDaemon, gameOptions, instance, permissions } from '#lib/testing/daemon.js';
import { choose, click } from '#lib/testing/interact.js';
import GameVersionPanel from './game-version-panel.svelte';

let daemon: FakeDaemon;

async function open(held: string[]) {
	session.permissions = permissions('inst-a', held);
	render(GameVersionPanel, { instance: instance() });
	if (held.includes(actions.gameUpdate)) await screen.findByLabelText('Game version');
}

const save = () => screen.getByRole('button', { name: 'Save game version' }) as HTMLButtonElement;
const patches = () => daemon.requests('PATCH', '/instances/inst-a');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/game/options', () => Response.json(gameOptions()));
	daemon.on('PATCH', '/instances/inst-a', () => Response.json(instance()));
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the game version panel', () => {
	it('saves a branch Steam lists, labelled with its description', async () => {
		await open([actions.view, actions.gameUpdate]);
		await choose(screen.getByLabelText('Game version'), 'default_old — Previous stable');
		await click(save());

		await vi.waitFor(() => expect(patches()).toHaveLength(1));
		expect(patches()[0].body).toEqual({ game_branch: 'default_old' });
		expect(await screen.findByText('Run the game update to switch')).toBeTruthy();
	});

	it('saves a password branch with its password', async () => {
		await open([actions.view, actions.gameUpdate]);
		await choose(screen.getByLabelText('Game version'), 'Password branch…');
		await fireEvent.input(screen.getByLabelText('Branch name'), {
			target: { value: 'public-test' }
		});
		await fireEvent.input(screen.getByLabelText('Branch password'), {
			target: { value: 'letmein' }
		});
		await click(save());

		await vi.waitFor(() => expect(patches()).toHaveLength(1));
		expect(patches()[0].body).toEqual({
			game_branch: 'public-test',
			game_branch_password: 'letmein'
		});
	});

	it('is hidden from a member who cannot update the game', async () => {
		await open([actions.view, actions.settings]);
		expect(screen.queryByText('Game version')).toBeNull();
	});
});
