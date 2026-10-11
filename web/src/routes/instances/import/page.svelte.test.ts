import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import type { ManifestPreview } from '$lib/api/manifest';
import { session } from '$lib/state/session.svelte';
import { FakeDaemon, gameOptions, job, permissions } from '$lib/testing/daemon';
import { socket } from '$lib/testing/socket';
import Page from './+page.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const preview: ManifestPreview = {
	name: 'Vanilla QoL',
	instance: {
		server_name: 'Viking Friends',
		world_name: 'Midgard',
		public: false,
		crossplay: false,
		mem_limit_mb: 4096,
		cpu_limit: null,
		backup_keep_cold: 7,
		backup_keep_hot: 3,
		backup_on_restart: false
	},
	mods: [{ full_name: 'Smoothbrain-Sailing', source: 'hexium', version: '1.1.9', available: true }],
	configs: [{ file: 'Sailing.cfg', size_bytes: 20 }],
	problems: []
};

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.permissions = permissions('inst-a', [], [actions.create]);
	daemon.on('GET', '/game/options', () => Response.json(gameOptions()));
	daemon.on('POST', '/instances/manifest/preview', () => Response.json(preview));
	daemon.on('POST', '/instances/import', () => Response.json(job()));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
});

afterEach(() => {
	session.permissions = null;
	socket.reset();
	vi.unstubAllGlobals();
});

describe('Import from a template code', () => {
	it('previews a pasted code and imports it with the chosen name and password', async () => {
		render(Page);
		await fireEvent.input(screen.getByLabelText('Template code'), {
			target: { value: '  valmin1:abc\n' }
		});
		await fireEvent.click(screen.getByRole('button', { name: 'Check code' }));
		await screen.findByText('Smoothbrain-Sailing');
		expect(daemon.requests('POST', '/instances/manifest/preview')[0].body).toEqual({
			code: 'valmin1:abc'
		});
		expect((screen.getByLabelText('Name') as HTMLInputElement).value).toBe('Vanilla QoL');
		expect(screen.getByText(/applied over the files the mods create/)).toBeTruthy();

		await fireEvent.input(screen.getByLabelText('Server password'), {
			target: { value: 'hunter2' }
		});
		await fireEvent.click(screen.getByRole('button', { name: 'Create server' }));
		await vi.waitFor(() => expect(daemon.requests('POST', '/instances/import')).toHaveLength(1));
		expect(daemon.requests('POST', '/instances/import')[0].body).toEqual({
			code: 'valmin1:abc',
			name: 'Vanilla QoL',
			password: 'hunter2',
			start_after_provision: false
		});
	});
});
