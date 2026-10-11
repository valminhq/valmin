import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { FakeDaemon, envelope } from '$lib/testing/daemon';
import PlayerHistory from './player-history.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const path = '/instances/inst-a/players/history';
let daemon: FakeDaemon;

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	daemon.on('GET', '/instances/inst-a/stats', () => Response.json({ available: false }));
});

afterEach(() => vi.unstubAllGlobals());

function page(from: string, to: string, items: unknown[], next: string | null) {
	return Response.json({
		items,
		next_cursor: next,
		total: null,
		range: {
			from,
			to,
			initial: {
				id: 'seed',
				observed_at: new Date(Date.parse(from) - 1000).toISOString(),
				players: 1
			}
		}
	});
}

it('waits for every history page before displaying summaries, then navigates ranges', async () => {
	let finish: ((response: Response) => void) | undefined;
	const nextPage = new Promise<Response>((resolve) => (finish = resolve));
	daemon.on('GET', path, (request) => {
		const from = request.query.get('from')!;
		const to = request.query.get('to')!;
		if (request.query.has('cursor')) return nextPage;
		return page(from, to, [], 'page-2');
	});
	render(PlayerHistory, { instanceId: 'inst-a' });
	await vi.waitFor(() => expect(daemon.requests('GET', path)).toHaveLength(2));
	expect(screen.queryByText('Peak players')).toBeNull();
	const second = daemon.requests('GET', path)[1];
	finish?.(page(second.query.get('from')!, second.query.get('to')!, [], null));
	expect(await screen.findByText('Peak players')).toBeTruthy();
	expect(screen.getByText('100.0%', { exact: false })).toBeTruthy();
	await fireEvent.click(screen.getByRole('button', { name: '7 days' }));
	await vi.waitFor(() => expect(daemon.requests('GET', path)).toHaveLength(4));
	expect(daemon.requests('GET', path)[2].query.get('cursor')).toBeNull();
	expect(screen.getByRole('button', { name: 'Previous period' })).toBeTruthy();
});

it('keeps the last complete chart visible when a refresh fails and retries explicitly', async () => {
	let fail = false;
	daemon.on('GET', path, (request) => {
		if (fail) return envelope(503, 'unavailable', 'History unavailable.');
		return page(request.query.get('from')!, request.query.get('to')!, [], null);
	});
	render(PlayerHistory, { instanceId: 'inst-a' });
	expect(await screen.findByText('Peak players')).toBeTruthy();
	fail = true;
	document.dispatchEvent(new Event('visibilitychange'));
	expect(await screen.findByRole('button', { name: 'Retry activity' })).toBeTruthy();
	expect(screen.getByText('Peak players')).toBeTruthy();
	fail = false;
	await fireEvent.click(screen.getByRole('button', { name: 'Retry activity' }));
	expect(await screen.findByText('Peak players')).toBeTruthy();
});

it('exposes gaps and interval details to keyboard and table readers', async () => {
	daemon.on('GET', path, (request) => {
		const from = request.query.get('from')!;
		const to = request.query.get('to')!;
		return Response.json({
			items: [
				{
					id: 'gap',
					observed_at: new Date(Date.parse(from) + 3_600_000).toISOString(),
					players: null
				}
			],
			next_cursor: null,
			range: { from, to, initial: { id: 'seed', observed_at: from, players: 1 } }
		});
	});
	render(PlayerHistory, { instanceId: 'inst-a' });
	const chart = await screen.findByRole('button', { name: /Player activity timeline/ });
	await fireEvent.keyDown(chart, { key: 'ArrowRight' });
	expect(screen.getByText(/1 players for 1 h 0 min/)).toBeTruthy();
	await fireEvent.keyDown(chart, { key: 'ArrowRight' });
	expect(screen.getByText(/not observed for/)).toBeTruthy();
	await fireEvent.click(screen.getByText(/View observations/));
	expect(screen.getByRole('table', { name: 'Observed player counts, newest first' })).toBeTruthy();
});
