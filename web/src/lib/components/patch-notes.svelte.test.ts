import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PatchNote } from '$lib/api/instances';
import { FakeDaemon, envelope } from '$lib/testing/daemon';
import { click, text } from '$lib/testing/interact';
import PatchNotes from './patch-notes.svelte';

let daemon: FakeDaemon;

const note: PatchNote = {
	title: 'Patch 1.0.20',
	url: 'https://store.steampowered.com/news/app/892970/view/1846651706676865',
	published_at: '2026-10-22T10:00:00Z',
	text: 'Trophy drop chance is now increased by star level of monsters'
};

const patches = () => daemon.requests('GET', '/game/patch-notes');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => vi.unstubAllGlobals());

describe('the patch notes', () => {
	// Steam is only asked once someone wants to read them.
	it('asks Steam only when the list is opened', async () => {
		daemon.on('GET', '/game/patch-notes', () => Response.json({ notes: [note] }));
		render(PatchNotes);
		expect(patches()).toHaveLength(0);

		await click(screen.getByText('Recent game patch notes'));

		expect(await screen.findByText('Patch 1.0.20')).toBeTruthy();
		expect(patches()).toHaveLength(1);
		expect(text(document.body)).toContain('Trophy drop chance');
		const link = screen.getByRole('link', { name: 'Open on Steam' }) as HTMLAnchorElement;
		expect(link.href).toBe(note.url);
	});

	it('says the notes are not matched to the build', async () => {
		daemon.on('GET', '/game/patch-notes', () => Response.json({ notes: [note] }));
		render(PatchNotes);
		await click(screen.getByText('Recent game patch notes'));
		expect(
			await screen.findByText(/Steam does not say which patch a build id belongs to/)
		).toBeTruthy();
	});

	it('falls back to a link when Steam cannot be reached', async () => {
		daemon.on('GET', '/game/patch-notes', () =>
			envelope(503, 'unavailable', "Steam's news could not be reached.")
		);
		render(PatchNotes);
		await click(screen.getByText('Recent game patch notes'));

		const link = (await screen.findByRole('link', {
			name: 'Read the patch notes on Steam'
		})) as HTMLAnchorElement;
		expect(link.href).toBe('https://store.steampowered.com/news/app/892970');
	});

	it('says so when Steam lists no patch notes', async () => {
		daemon.on('GET', '/game/patch-notes', () => Response.json({ notes: [] }));
		render(PatchNotes);
		await click(screen.getByText('Recent game patch notes'));
		expect(await screen.findByText(/lists no recent patch notes/)).toBeTruthy();
	});
});
