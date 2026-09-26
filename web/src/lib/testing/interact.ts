import { fireEvent, screen } from '@testing-library/svelte';
import { tick } from 'svelte';

/** Rendered text with the template's line breaks collapsed, as a reader sees it. */
export const text = (el: Element | null | undefined) =>
	(el?.textContent ?? '').replace(/\s+/g, ' ');

/**
 * Picks `option` from a bits-ui select the way a mouse does: it opens on pointerdown and
 * selects on pointerup, not on click, so a plain click event opens nothing and picks nothing.
 */
export async function choose(trigger: HTMLElement, option: string): Promise<void> {
	await fireEvent.pointerDown(trigger, { button: 0, pointerType: 'mouse' });
	const item = await screen.findByRole('option', { name: option });
	await fireEvent.pointerUp(item, { button: 0, pointerType: 'mouse' });
}

/**
 * Clicks the way a browser does. `fireEvent.click` dispatches straight to the listeners, so a
 * disabled button still runs its handler under it; a browser, and `element.click()`, run
 * nothing. A test that pressed a disabled confirm with fireEvent would pass against a page
 * whose gate is broken.
 */
export async function click(el: HTMLElement): Promise<void> {
	el.click();
	await tick();
}
