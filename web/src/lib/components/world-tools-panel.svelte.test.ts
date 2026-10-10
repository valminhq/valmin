import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions, type Instance } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, instance, job, permissions } from '$lib/testing/daemon';
import { click } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import WorldToolsPanel from './world-tools-panel.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const capsPath = '/instances/inst-a/capabilities';
let daemon: FakeDaemon;

function capabilities(tools: { upgrade_world: boolean; fresh_world: boolean }, rcon = true) {
	daemon.on('GET', capsPath, () =>
		Response.json({
			command_channel: rcon ? 'rcon' : 'none',
			detected: true,
			allowed_commands: [],
			allowed_actions: [],
			world_tools: tools
		})
	);
}

async function open(
	row: Partial<Instance> = {},
	held: string[] = [actions.view, actions.backupsRestore]
) {
	session.permissions = permissions('inst-a', held);
	render(WorldToolsPanel, { instance: instance(row) });
	if (held.includes(actions.backupsRestore)) {
		await vi.waitFor(() => expect(daemon.requests('GET', capsPath)).toHaveLength(1));
	}
}

const button = (name: string) => screen.getByRole('button', { name }) as HTMLButtonElement;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	socket.reset();
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the world tools panel', () => {
	it('is not shown to someone who may not restore backups', async () => {
		await open({}, [actions.view, actions.backupsCreate]);

		expect(screen.queryByText('World tools')).toBeNull();
		expect(daemon.requests('GET', capsPath)).toHaveLength(0);
	});

	it('greys out a tool whose mod is missing and links to it in the mods page', async () => {
		capabilities({ upgrade_world: false, fresh_world: true });
		await open();

		await vi.waitFor(() =>
			expect(screen.getByText(/Install JereKuusela-Upgrade_World/)).toBeTruthy()
		);
		expect(button('Clean world').disabled).toBe(true);
		expect(button('Run FreshWorld').disabled).toBe(false);
		const link = screen.getByRole('link', { name: 'Find it in mods' }) as HTMLAnchorElement;
		expect(link.getAttribute('href')).toContain('/instances/inst-a/mods?q=Upgrade_World');
	});

	it('asks for the RCON mod when it is missing', async () => {
		capabilities({ upgrade_world: true, fresh_world: true }, false);
		await open();

		await vi.waitFor(() =>
			expect(screen.getAllByText(/Install Tristan-ValheimRcon/)).toHaveLength(2)
		);
		expect(button('Run FreshWorld').disabled).toBe(true);
	});

	it('asks for the server to be stopped first', async () => {
		capabilities({ upgrade_world: true, fresh_world: true });
		await open({ state: 'running' });

		await vi.waitFor(() => expect(screen.getAllByText(/Stop the server first/)).toHaveLength(2));
		expect(button('Clean world').disabled).toBe(true);
	});

	it('runs a world clean once the world name is typed back', async () => {
		capabilities({ upgrade_world: true, fresh_world: false });
		daemon.on('POST', '/instances/inst-a/world-tools', () =>
			Response.json(job({ job_id: 'job-1', kind: 'world_tool', status: 'queued' }), {
				status: 202
			})
		);
		daemon.on('GET', '/jobs/job-1', () => Response.json(job({ kind: 'world_tool' })));
		await open();

		await vi.waitFor(() => expect(button('Clean world').disabled).toBe(false));
		await click(button('Clean world'));
		await fireEvent.input(await screen.findByLabelText(/to confirm/), {
			target: { value: 'MyWorld' }
		});
		await click(button('Clean the world'));

		await vi.waitFor(() =>
			expect(daemon.requests('POST', '/instances/inst-a/world-tools')).toHaveLength(1)
		);
		expect(daemon.requests('POST', '/instances/inst-a/world-tools')[0].body).toEqual({
			tool: 'upgrade_world',
			action: 'world_clean'
		});
	});

	it('refuses a worldgen upgrade before sending it', async () => {
		capabilities({ upgrade_world: true, fresh_world: false });
		await open();

		await fireEvent.input(screen.getByLabelText('Operation'), {
			target: { value: 'legacy_worldgen' }
		});
		await vi.waitFor(() => expect(button('Upgrade').disabled).toBe(true));
		await fireEvent.input(screen.getByLabelText('Operation'), { target: { value: 'tarpits' } });
		await vi.waitFor(() => expect(button('Upgrade').disabled).toBe(false));
	});
});
