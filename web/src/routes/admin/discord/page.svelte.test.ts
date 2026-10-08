import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DiscordSettings } from '$lib/api/admin';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import Page from './+page.svelte';

let daemon: FakeDaemon;

function settings(overrides: Partial<DiscordSettings> = {}): DiscordSettings {
	return {
		configured: true,
		enabled: true,
		links: [
			{
				id: 'l-1',
				guild_id: '123456789012345678',
				channel_id: '',
				allow_start: true,
				instance_ids: ['inst-a']
			}
		],
		status: { state: 'connected', bot_name: 'ValminBot', application_id: 'app-1' },
		admin_ids: [],
		timezone: 'Europe/Kyiv',
		invite_url: 'https://discord.com/oauth2/authorize?client_id=app-1',
		...overrides
	};
}

async function open(global: string[], row = settings()) {
	daemon.on('GET', '/admin/discord', () => Response.json(row));
	daemon.on('GET', '/instances', () =>
		Response.json({
			items: [instance({ id: 'inst-a', name: 'alpha' }), instance({ id: 'inst-b', name: 'bravo' })],
			next_cursor: null
		})
	);
	daemon.on('PUT', '/admin/discord', () => Response.json(row));
	session.permissions = permissions('', [], global);
	render(Page);
	if (global.includes(actions.panelSettings))
		await screen.findByRole('button', { name: 'Save Discord settings' });
}

function save() {
	return click(screen.getByRole('button', { name: 'Save Discord settings' }));
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the Discord bot screen', () => {
	it('is closed without panel.settings', async () => {
		await open([actions.view]);
		expect(screen.getByText(/do not have permission/)).toBeTruthy();
		expect(daemon.requests('GET', '/admin/discord')).toHaveLength(0);
	});

	it('shows who the bot is and how to invite it', async () => {
		await open([actions.panelSettings]);
		expect(screen.getByRole('status').textContent).toContain('Connected as ValminBot');
		expect(screen.getByRole('link', { name: 'Invite the bot to a Discord server' })).toBeTruthy();
	});

	it('saves the whole document and keeps an untouched token', async () => {
		await open([actions.panelSettings]);
		await click(screen.getByRole('checkbox', { name: 'bravo' }));
		await fireEvent.input(screen.getByLabelText('Channel ID (optional)'), {
			target: { value: ' 223456789012345678 ' }
		});
		await save();

		await vi.waitFor(() => expect(daemon.requests('PUT', '/admin/discord')).toHaveLength(1));
		expect(daemon.requests('PUT', '/admin/discord')[0].body).toEqual({
			enabled: true,
			links: [
				{
					guild_id: '123456789012345678',
					channel_id: '223456789012345678',
					allow_start: true,
					instance_ids: ['inst-a', 'inst-b']
				}
			],
			admin_ids: [],
			timezone: 'Europe/Kyiv'
		});
	});

	it('sends a new token and a new link', async () => {
		await open([actions.panelSettings], settings({ configured: false, links: [] }));
		await fireEvent.input(screen.getByLabelText('Bot token'), { target: { value: 'tok' } });
		await click(screen.getByRole('button', { name: 'Add link' }));
		await fireEvent.input(screen.getByLabelText('Discord server ID'), {
			target: { value: '123456789012345678' }
		});
		await click(screen.getByRole('switch', { name: 'Allow /start' }));
		await save();

		await vi.waitFor(() => expect(daemon.requests('PUT', '/admin/discord')).toHaveLength(1));
		expect(daemon.requests('PUT', '/admin/discord')[0].body).toEqual({
			token: 'tok',
			enabled: true,
			links: [
				{ guild_id: '123456789012345678', channel_id: '', allow_start: false, instance_ids: [] }
			],
			admin_ids: [],
			timezone: expect.any(String)
		});
	});

	it('sends the bot admins one per line and the zone typed times are read in', async () => {
		await open([actions.panelSettings]);
		await fireEvent.input(screen.getByLabelText('User or role IDs, one per line'), {
			target: { value: ' 111111111111111111\n\n222222222222222222 ' }
		});
		await fireEvent.input(screen.getByLabelText('Time zone of typed times'), {
			target: { value: 'UTC' }
		});
		await save();

		await vi.waitFor(() => expect(daemon.requests('PUT', '/admin/discord')).toHaveLength(1));
		expect(daemon.requests('PUT', '/admin/discord')[0].body).toMatchObject({
			admin_ids: ['111111111111111111', '222222222222222222'],
			timezone: 'UTC'
		});
	});
});
