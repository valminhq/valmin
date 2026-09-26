import { afterAll } from 'vitest';

// jsdom lays nothing out, so it has no scrollIntoView; the screens call it to bring a rejected
// field into view before focusing it. A no-op keeps that path running, and the focus it leads
// to is still real and assertable.
Element.prototype.scrollIntoView = () => {};

// Nor pointer capture, which bits-ui's select takes on pointerdown before it opens.
Element.prototype.hasPointerCapture = () => false;
Element.prototype.setPointerCapture = () => {};
Element.prototype.releasePointerCapture = () => {};

// Nor media queries, which the theme watcher and the charts read at load. Nothing matches:
// no dark mode, no reduced motion.
window.matchMedia = (query: string) =>
	({
		matches: false,
		media: query,
		onchange: null,
		addEventListener: () => {},
		removeEventListener: () => {},
		addListener: () => {},
		removeListener: () => {},
		dispatchEvent: () => false
	}) as MediaQueryList;

// Nor ResizeObserver, which the charts and the console's virtual list size themselves with.
// jsdom has no layout, so there is nothing for it to report.
globalThis.ResizeObserver = class {
	observe() {}
	unobserve() {}
	disconnect() {}
};

// bits-ui restores the body's scroll style 24ms after the last dialog closes. A file whose
// final test closes one would otherwise tear the DOM down under that timer. Once per file,
// after its last test: this drains a known timer, it does not wait for work under test.
afterAll(() => new Promise((done) => setTimeout(done, 50)));
