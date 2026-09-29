import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConsoleBuffer } from '$lib/state/console.svelte';
import { topics } from '$lib/socket/messages';
import { click } from '$lib/testing/interact';
import { socket } from '$lib/testing/socket';
import ConsoleView from './console-view.svelte';

vi.mock('$lib/socket/index.svelte', () => import('$lib/testing/socket'));

const ROW_HEIGHT = 20;
const VIEWPORT = 400;

let scrollTo: ReturnType<typeof vi.fn>;

// jsdom lays nothing out, so the scroller is given a viewport, a scroll height that follows the
// virtual list's own total, and a scrollTo that clamps like a browser and reports a scroll only
// when the position actually moved.
beforeEach(() => {
	vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(VIEWPORT);
	vi.spyOn(Element.prototype, 'clientHeight', 'get').mockReturnValue(VIEWPORT);
	vi.spyOn(Element.prototype, 'scrollHeight', 'get').mockImplementation(function (this: Element) {
		return parseInt((this.firstElementChild as HTMLElement | null)?.style.height || '0', 10);
	});
	scrollTo = vi.fn(function (this: Element, options: ScrollToOptions) {
		const max = Math.max(this.scrollHeight - VIEWPORT, 0);
		const top = Math.min(Math.max(options.top ?? 0, 0), max);
		if (top === this.scrollTop) return;
		this.scrollTop = top;
		this.dispatchEvent(new Event('scroll'));
	});
	Object.defineProperty(Element.prototype, 'scrollTo', {
		value: scrollTo,
		configurable: true,
		writable: true
	});
});

afterEach(() => {
	vi.restoreAllMocks();
	Reflect.deleteProperty(Element.prototype, 'scrollTo');
	Reflect.deleteProperty(navigator, 'clipboard');
	document.getSelection()?.removeAllRanges();
	socket.reset();
});

function clipboard(writeText: (text: string) => Promise<void>) {
	Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
}

function deliver(message: Record<string, unknown>) {
	socket.push(topics.console('i1'), message as never);
}

/** Renders a console holding `lines`, one per row, once they have been published. */
async function open(lines: string[]) {
	const buffer = new ConsoleBuffer('i1');
	buffer.open();
	render(ConsoleView, { buffer, commandChannel: 'none', canSend: false, running: false });
	lines.forEach((line, index) =>
		deliver({
			type: 'console',
			instance: 'i1',
			seq: index + 1,
			ts: '2026-08-31T10:00:00Z',
			stream: 'stdout',
			line
		})
	);
	await vi.waitFor(() => expect(buffer.rows).toHaveLength(lines.length));
	await vi.waitFor(() => expect(rows().length).toBeGreaterThan(0));
	return buffer;
}

const log = () => screen.getByRole('log');
const rows = () => Array.from(log().firstElementChild?.children ?? []) as HTMLElement[];
const marks = () => Array.from(log().querySelectorAll('mark')).map((mark) => mark.textContent);
const activeRow = () => log().querySelector('[aria-current="true"]');
const searchBox = () => screen.getByLabelText('Search the console') as HTMLInputElement;
const search = (value: string) => fireEvent.input(searchBox(), { target: { value } });
const press = (key: string, init: KeyboardEventInit = {}) =>
	fireEvent.keyDown(searchBox(), { key, ...init });
const disabled = (name: string) => screen.getByRole('button', { name }).matches(':disabled');

/** Sixty lines with three hits, spread wider than the virtual list's rendered window. */
const sixty = Array.from({ length: 60 }, (_, index) => `line ${index}`);
sixty[5] = 'ERROR: disk almost full';
sixty[30] = 'an error occurred';
sixty[40] = 'Error twice, error';

describe('searching the console', () => {
	it('marks every hit ignoring case, counts the matching lines, and keeps the other lines', async () => {
		await open(sixty);
		await search('error');

		expect(screen.getByText('1 of 3')).toBeTruthy();
		expect(marks()).toEqual(['ERROR', 'error']);
		expect(screen.getByText('line 4')).toBeTruthy();
		expect(activeRow()?.textContent).toContain('ERROR: disk almost full');
	});

	it('moves to the next and previous match with Enter and Shift+Enter, wrapping at both ends', async () => {
		await open(sixty);
		await search('error');

		await press('Enter');
		expect(screen.getByText('2 of 3')).toBeTruthy();
		expect(activeRow()?.textContent).toContain('an error occurred');

		await press('Enter');
		expect(screen.getByText('3 of 3')).toBeTruthy();
		expect(activeRow()?.textContent).toContain('Error twice, error');

		await press('Enter');
		expect(screen.getByText('1 of 3')).toBeTruthy();
		expect(activeRow()?.textContent).toContain('ERROR: disk almost full');

		await press('Enter', { shiftKey: true });
		expect(screen.getByText('3 of 3')).toBeTruthy();
		expect(activeRow()?.textContent).toContain('Error twice, error');
	});

	it('steps with the Next and Previous buttons', async () => {
		await open(sixty);
		await search('error');

		await click(screen.getByRole('button', { name: 'Next match' }));
		expect(screen.getByText('2 of 3')).toBeTruthy();
		await click(screen.getByRole('button', { name: 'Previous match' }));
		expect(screen.getByText('1 of 3')).toBeTruthy();
	});

	it('scrolls the match into the rendered window by row index and pauses auto-scroll', async () => {
		await open(sixty);
		// Following the tail, the first row is far outside the rendered window.
		expect(screen.queryByText('line 4')).toBeNull();
		expect(screen.queryByText('Auto-scroll paused')).toBeNull();

		await search('error');
		expect(scrollTo).toHaveBeenLastCalledWith(expect.objectContaining({ top: 0 }));
		expect(screen.getByText('Auto-scroll paused')).toBeTruthy();
		expect(log().scrollTop).toBe(0);

		await press('Enter');
		await press('Enter');
		const top = 40 * ROW_HEIGHT;
		expect(log().scrollTop).toBeLessThanOrEqual(top);
		expect(log().scrollTop + VIEWPORT).toBeGreaterThan(top);
		expect(screen.getByText('Auto-scroll paused')).toBeTruthy();
	});

	it('leaves the scroll alone when the next match is already on screen', async () => {
		const lines = sixty.slice();
		lines[8] = 'error near the first';
		await open(lines);
		await search('error');

		scrollTo.mockClear();
		await press('Enter');
		expect(activeRow()?.textContent).toContain('error near the first');
		expect(scrollTo).not.toHaveBeenCalled();
	});

	it('keeps the active match and the paused scroll while new lines arrive', async () => {
		await open(sixty);
		await search('error');
		await press('Enter');

		deliver({
			type: 'console',
			instance: 'i1',
			seq: 61,
			ts: '2026-08-31T10:00:00Z',
			stream: 'stdout',
			line: 'another error'
		});
		await vi.waitFor(() => expect(screen.getByText('2 of 4')).toBeTruthy());
		expect(activeRow()?.textContent).toContain('an error occurred');
		expect(screen.getByText('Auto-scroll paused')).toBeTruthy();
	});

	it('keeps whitespace inside a highlighted line', async () => {
		await open(['a  b   c', 'other']);
		await search('b');

		expect(marks()).toEqual(['b']);
		expect(rows()[0].textContent).toContain('a  b   c');
	});

	it('clears the search with Escape', async () => {
		await open(sixty);
		await search('error');
		expect(marks().length).toBeGreaterThan(0);

		await press('Escape');
		expect(searchBox().value).toBe('');
		expect(marks()).toEqual([]);
		expect(activeRow()).toBeNull();
		expect(screen.queryByText(/ of \d+$/)).toBeNull();
	});

	it('says when nothing matches and disables stepping', async () => {
		await open(sixty);
		await search('nothing like this');

		expect(screen.getByText('No matches')).toBeTruthy();
		expect(disabled('Next match')).toBe(true);
		expect(disabled('Previous match')).toBe(true);
	});

	it('does not match the notices between lines', async () => {
		await open(['one']);
		deliver({ type: 'gap', topic: topics.console('i1'), dropped: 3, from_seq: 2 });
		await vi.waitFor(() => expect(screen.getByText(/3 lines dropped/)).toBeTruthy());

		await search('dropped');
		expect(screen.getByText('No matches')).toBeTruthy();
	});
});

describe('copying the selected text', () => {
	const COPY = 'Copy selected text';

	function select(node: Node) {
		const range = document.createRange();
		range.selectNodeContents(node);
		const selection = document.getSelection()!;
		selection.removeAllRanges();
		selection.addRange(range);
		document.dispatchEvent(new Event('selectionchange'));
	}

	/** Selects the last line's text and waits for the button to notice. */
	async function selectLastLine() {
		select(screen.getByText('line 59'));
		await vi.waitFor(() => expect(disabled(COPY)).toBe(false));
	}

	it('is disabled until text inside the log is selected', async () => {
		await open(sixty);
		expect(disabled(COPY)).toBe(true);

		await selectLastLine();

		document.getSelection()?.removeAllRanges();
		document.dispatchEvent(new Event('selectionchange'));
		await vi.waitFor(() => expect(disabled(COPY)).toBe(true));
	});

	it('ignores a selection outside the log', async () => {
		await open(sixty);
		select(screen.getByText('Search the console'));

		await new Promise((resolve) => setTimeout(resolve, 0));
		expect(disabled(COPY)).toBe(true);
	});

	it('copies exactly what is selected and says so', async () => {
		const writeText = vi.fn(() => Promise.resolve());
		clipboard(writeText);
		await open(sixty);
		await selectLastLine();

		await click(screen.getByRole('button', { name: COPY }));
		expect(writeText).toHaveBeenCalledWith('line 59');
		await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Copied' })).toBeTruthy());
	});

	it('says the copy was refused and what to do instead', async () => {
		clipboard(() => Promise.reject(new DOMException('Not allowed', 'NotAllowedError')));
		await open(sixty);
		await selectLastLine();

		await click(screen.getByRole('button', { name: COPY }));
		expect(await screen.findByText(/Select the text and copy it by hand\./)).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Copied' })).toBeNull();
	});
});
