import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import type { ManifestPreview, TemplateCatalogue } from '#lib/api/manifest.js';
import { session } from '#lib/state/session.svelte.js';
import { instanceList } from '#lib/state/instances.svelte.js';
import { FakeDaemon, gameOptions, instance, job, permissions } from '#lib/testing/daemon.js';
import { socket } from '#lib/testing/socket.js';
import Page from './+page.svelte';

vi.mock('#lib/socket/index.svelte.js', () => import('#lib/testing/socket.js'));

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
	configs: [
		{
			file: 'Sailing.cfg',
			size_bytes: 20,
			settings: [
				{ key: 'General.Speed', value: '2', secret: false, suspicious: false },
				{ key: 'General.Hook', value: 'https://example.com/h', secret: false, suspicious: true }
			]
		}
	],
	problems: []
};

const source = 'https://github.com/valminhq/valmin-presets/archive/refs/heads/main.tar.gz';

const catalogue: TemplateCatalogue = {
	sources: [{ url: source, fetched_at: '2026-10-11T12:00:00Z', error: null }],
	templates: [
		{
			source,
			slug: 'vanilla-qol',
			name: 'Vanilla QoL',
			description: 'Less inventory juggling.',
			mods: 1,
			code: 'valmin1:qol'
		},
		{
			source,
			slug: 'hardcore',
			name: 'Hardcore',
			description: 'No map, hard combat.',
			mods: 0,
			code: 'valmin1:hard'
		}
	],
	problems: [{ source, path: 'templates/broken/template.yaml', detail: 'name is missing' }]
};

let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	session.permissions = permissions('inst-a', [], [actions.create]);
	daemon.on('GET', '/game/options', () => Response.json(gameOptions()));
	daemon.on('GET', '/templates', () => Response.json(catalogue));
	daemon.on('POST', '/instances/manifest/preview', () => Response.json(preview));
	daemon.on('POST', '/instances/import', () => Response.json(job()));
	daemon.on('GET', '/jobs/job-1', () => Response.json(job()));
});

afterEach(() => {
	session.permissions = null;
	instanceList.release();
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
		expect(screen.getByText('General.Speed')).toBeTruthy();
		expect(screen.getByText('= 2')).toBeTruthy();
		expect(screen.getAllByText('looks secret')).toHaveLength(1);

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

describe('the new server name', () => {
	// A template usually carries the name of a server that still exists, and a taken name is
	// refused at the bottom of a long page.
	it('starts unused and shows a refusal under the field', async () => {
		daemon.on('GET', '/instances', () =>
			Response.json({ items: [instance({ name: 'Vanilla QoL' })], next_cursor: null })
		);
		daemon.on('POST', '/instances/import', () =>
			Response.json(
				{
					error: {
						code: 'name_taken',
						message: 'That name is already in use.',
						request_id: 'req-test',
						details: { field: 'name' }
					}
				},
				{ status: 409 }
			)
		);
		render(Page);
		await fireEvent.input(screen.getByLabelText('Template code'), {
			target: { value: 'valmin1:abc' }
		});
		await fireEvent.click(screen.getByRole('button', { name: 'Check code' }));
		await screen.findByText('Smoothbrain-Sailing');
		const name = screen.getByLabelText('Name') as HTMLInputElement;
		expect(name.value).toBe('Vanilla QoL 2');

		await fireEvent.input(name, { target: { value: 'Vanilla QoL' } });
		await fireEvent.input(screen.getByLabelText('Server password'), {
			target: { value: 'hunter2' }
		});
		await fireEvent.click(screen.getByRole('button', { name: 'Create server' }));
		await vi.waitFor(() => expect(name.getAttribute('aria-invalid')).toBe('true'));
		expect(screen.getAllByText('That name is already in use.').length).toBeGreaterThan(0);
	});
});

describe('Import from a mod manager profile code', () => {
	it('sends a pasted r2modman or Gale code as it is, trimmed', async () => {
		const key = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
		render(Page);
		await fireEvent.input(screen.getByLabelText('Template code'), {
			target: { value: ` ${key}\n` }
		});
		await fireEvent.click(screen.getByRole('button', { name: 'Check code' }));
		await screen.findByText('Smoothbrain-Sailing');
		expect(daemon.requests('POST', '/instances/manifest/preview')[0].body).toEqual({ code: key });
	});
});

describe('Shared templates', () => {
	it('lists, filters and previews a template from a repository', async () => {
		render(Page);
		await screen.findByText('Less inventory juggling.');
		expect(screen.getByText('Hardcore')).toBeTruthy();
		expect(screen.getAllByText(/github\.com\/valminhq\/valmin-presets/).length).toBeGreaterThan(0);
		expect(screen.getByText(/1 template could not be read/)).toBeTruthy();

		await fireEvent.input(screen.getByLabelText('Filter templates'), {
			target: { value: 'juggling' }
		});
		expect(screen.queryByText('Hardcore')).toBeNull();

		await fireEvent.click(screen.getByRole('button', { name: 'Use Vanilla QoL' }));
		await screen.findByText('Smoothbrain-Sailing');
		expect(daemon.requests('POST', '/instances/manifest/preview')[0].body).toEqual({
			code: 'valmin1:qol'
		});
		expect((screen.getByLabelText('Template code') as HTMLTextAreaElement).value).toBe(
			'valmin1:qol'
		);
	});

	it('reads the repositories again on Refresh', async () => {
		render(Page);
		await screen.findByText('Hardcore');
		await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
		await vi.waitFor(() => expect(daemon.requests('GET', '/templates')).toHaveLength(2));
		expect(daemon.requests('GET', '/templates')[1].query.get('refresh')).toBe('true');
	});
});
