import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { actions, type Instance } from '$lib/api/instances';
import type { MyPermissions } from '$lib/api/types';
import { instanceList } from '$lib/state/instances.svelte';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance } from '$lib/testing/daemon';
import { socket } from '$lib/testing/socket';
import Layout from './+layout.svelte';

const app = vi.hoisted(() => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/backups') }
}));

vi.mock('$app/state', () => app);
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/paths', () => ({
	base: '',
	resolve: (route: string, params?: { id: string }) => route.replace('[id]', params?.id ?? '')
}));
vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const compare = [actions.settings, actions.modsList, actions.configRead];
const servers = [
	instance({ id: 'inst-a', name: 'Live' }),
	instance({ id: 'inst-b', name: 'Test' }),
	instance({ id: 'inst-c', name: 'Scratch' })
];

function grants(held: Record<string, string[]>): MyPermissions {
	return {
		user_id: 'u-test',
		role: 'member',
		allowed_actions: [],
		instances: Object.entries(held).map(([instance_id, allowed_actions]) => ({
			instance_id,
			allowed_actions
		}))
	};
}

let daemon: FakeDaemon;

async function open(
	pathname: string,
	held: Record<string, string[]>,
	listed: Instance[] = servers
) {
	app.page.url = new URL(`http://localhost${pathname}`);
	daemon.on('GET', '/instances', () => Response.json({ items: listed, next_cursor: null }));
	session.permissions = grants(held);
	render(Layout, {
		props: { children: createRawSnippet(() => ({ render: () => '<p>section</p>' })) }
	});
	await vi.waitFor(() => expect(daemon.requests('GET', '/instances')).toHaveLength(1));
	await vi.waitFor(() =>
		expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Live')
	);
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.user = {
		id: 'u-test',
		username: 'ada',
		role: 'member',
		disabled: false,
		owner: false,
		created_at: '2026-09-01T00:00:00Z',
		last_login_at: null,
		timezone: ''
	};
	vi.mocked(goto).mockClear();
});

afterEach(() => {
	session.user = null;
	session.permissions = null;
	instanceList.release();
	socket.reset();
	vi.unstubAllGlobals();
});

describe('the server switcher', () => {
	it('is absent when there is only one server', async () => {
		await open('/instances/inst-a/backups', { 'inst-a': [actions.backupsList] }, [servers[0]]);

		expect(screen.queryByRole('combobox', { name: 'Switch server' })).toBeNull();
	});

	it('lists every server the caller can see and shows the current one', async () => {
		await open('/instances/inst-a/backups', { 'inst-a': [actions.backupsList] });

		const select = await screen.findByRole<HTMLSelectElement>('combobox', {
			name: 'Switch server'
		});
		expect([...select.options].map((option) => option.textContent)).toEqual([
			'Live',
			'Test',
			'Scratch'
		]);
		expect(select.value).toBe('inst-a');
	});

	it('opens the same section of the chosen server when it is permitted there', async () => {
		await open('/instances/inst-a/backups', {
			'inst-a': [actions.backupsList],
			'inst-b': [actions.backupsList]
		});

		const select = await screen.findByRole('combobox', { name: 'Switch server' });
		await fireEvent.change(select, { target: { value: 'inst-b' } });

		expect(goto).toHaveBeenCalledExactlyOnceWith('/instances/inst-b/backups');
	});

	it('lands on the overview of a server that does not offer the section', async () => {
		await open('/instances/inst-a/backups', {
			'inst-a': [actions.backupsList],
			'inst-c': [actions.view]
		});

		const select = await screen.findByRole('combobox', { name: 'Switch server' });
		await fireEvent.change(select, { target: { value: 'inst-c' } });

		expect(goto).toHaveBeenCalledExactlyOnceWith('/instances/inst-c');
	});
});

describe('the compare link', () => {
	it('is a plain link to the comparison for a caller who may compare', async () => {
		await open('/instances/inst-a', { 'inst-a': compare });

		const link = screen.getByRole('link', { name: 'Compare with another server' });
		expect(link.getAttribute('href')).toBe('/instances/inst-a/compare');
		expect(screen.getByRole('navigation', { name: 'Server sections' }).textContent).not.toContain(
			'Compare'
		);
	});

	it('is absent on the comparison page itself', async () => {
		await open('/instances/inst-a/compare', { 'inst-a': compare });

		expect(screen.queryByRole('link', { name: 'Compare with another server' })).toBeNull();
	});

	it('is absent without settings, mods and configuration access', async () => {
		await open('/instances/inst-a', { 'inst-a': [actions.settings, actions.modsList] });

		expect(screen.queryByRole('link', { name: 'Compare with another server' })).toBeNull();
	});
});
