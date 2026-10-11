import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeDaemon } from '$lib/testing/daemon';
import { text } from '$lib/testing/interact';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({ page: { params: { id: 'inst-a' } } }));

const PATH = '/public/status/inst-a';
const AUTO_REFRESH_KEY = 'valmin.status.auto-refresh';

let daemon: FakeDaemon;
let stored: Map<string, string>;

/** The public payload, with only the fields a test cares about overridden. */
function status(overrides: Record<string, unknown> = {}) {
	return { name: 'My Server', online: false, players: null, observed_at: null, ...overrides };
}

/** Renders the page against a daemon serving body, once the first read has landed. */
async function open(body = status()) {
	daemon.on('GET', PATH, () => Response.json(body));
	render(Page);
	await screen.findByRole('heading', { name: 'My Server' });
}

const reads = () => daemon.requests('GET', PATH).length;
const autoRefresh = () =>
	screen.getByRole('checkbox', { name: 'Refresh automatically' }) as HTMLInputElement;

/** Sets what the page reads as the tab's visibility. */
function visibility(state: DocumentVisibilityState) {
	Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
}

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
	stored = new Map();
	vi.stubGlobal('localStorage', {
		getItem: (key: string) => stored.get(key) ?? null,
		setItem: (key: string, value: string) => void stored.set(key, value)
	});
	visibility('visible');
});

afterEach(() => {
	vi.useRealTimers();
	vi.unstubAllGlobals();
});

describe('the public status page', () => {
	it('renders the notice and connection guidance as plain text with their line breaks', async () => {
		await open(
			status({
				notice: '<b>Down</b> for the update\nuntil 20:00',
				connect_info: 'Join play.example:2456\nAsk in chat for the password.'
			})
		);

		const notice = screen.getByTestId('notice');
		expect(notice.textContent?.trim()).toBe('<b>Down</b> for the update\nuntil 20:00');
		expect(notice.querySelector('b'), 'markup stays text').toBeNull();
		expect(screen.getByRole('heading', { name: 'How to join' })).toBeTruthy();
		expect(screen.getByTestId('connect-info').textContent?.trim()).toBe(
			'Join play.example:2456\nAsk in chat for the password.'
		);
	});

	it('shows neither block when the administrator wrote nothing', async () => {
		await open();
		expect(screen.queryByTestId('notice')).toBeNull();
		expect(screen.queryByText('How to join')).toBeNull();
	});

	it.each([
		[status({ activity: 'updating' }), 'Maintenance in progress: updating'],
		[status(), 'Offline'],
		[status({ online: true, players: 2 }), 'Online']
	])('names the server state for %j', async (body, want) => {
		await open(body);
		expect(text(document.querySelector('main')), want).toContain(want);
		if (want !== 'Offline') expect(screen.queryByText('Offline')).toBeNull();
	});

	describe('automatic refresh', () => {
		beforeEach(() => {
			vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
		});

		it('is off by default and reads only once', async () => {
			await open();
			expect(autoRefresh().checked).toBe(false);

			vi.advanceTimersByTime(90_000);
			expect(reads()).toBe(1);
		});

		it('re-reads every 30 seconds once turned on, and remembers the choice', async () => {
			await open();
			await fireEvent.click(autoRefresh());
			expect(stored.get(AUTO_REFRESH_KEY)).toBe('true');

			vi.advanceTimersByTime(29_999);
			expect(reads()).toBe(1);
			vi.advanceTimersByTime(1);
			await vi.waitFor(() => expect(reads()).toBe(2));
			await screen.findByRole('button', { name: 'Refresh status' });
			vi.advanceTimersByTime(30_000);
			await vi.waitFor(() => expect(reads()).toBe(3));

			await fireEvent.click(autoRefresh());
			expect(stored.get(AUTO_REFRESH_KEY)).toBe('false');
			vi.advanceTimersByTime(90_000);
			expect(reads()).toBe(3);
		});

		it('starts on when this browser chose it before', async () => {
			stored.set(AUTO_REFRESH_KEY, 'true');
			await open();
			expect(autoRefresh().checked).toBe(true);

			vi.advanceTimersByTime(30_000);
			await vi.waitFor(() => expect(reads()).toBe(2));
		});

		it('pauses while the tab is hidden', async () => {
			stored.set(AUTO_REFRESH_KEY, 'true');
			await open();

			visibility('hidden');
			vi.advanceTimersByTime(90_000);
			expect(reads()).toBe(1);

			visibility('visible');
			vi.advanceTimersByTime(30_000);
			await vi.waitFor(() => expect(reads()).toBe(2));
		});

		it('works without storage, off by default', async () => {
			const blocked = () => {
				throw new Error('blocked');
			};
			vi.stubGlobal('localStorage', { getItem: blocked, setItem: blocked });
			await open();
			expect(autoRefresh().checked).toBe(false);

			await fireEvent.click(autoRefresh());
			expect(autoRefresh().checked).toBe(true);
			vi.advanceTimersByTime(30_000);
			await vi.waitFor(() => expect(reads()).toBe(2));
		});
	});

	it('keeps the manual refresh and the last-checked time', async () => {
		await open();
		expect(screen.getByText(/^Last checked/)).toBeTruthy();

		await fireEvent.click(screen.getByRole('button', { name: 'Refresh status' }));
		await vi.waitFor(() => expect(reads()).toBe(2));
	});
});
