import {
	Virtualizer,
	type VirtualItem,
	elementScroll,
	observeElementOffset,
	observeElementRect
} from '@tanstack/virtual-core';

/**
 * A runes wrapper over TanStack's virtual-core.
 *
 * `↯` **`@tanstack/virtual-core`, not `@tanstack/svelte-virtual`** (ADR-100). `06 §4` and the
 * M1 plan both name the Svelte adapter; it cannot be used here. It hands back a Svelte 4
 * `Readable`, so every consuming component would need `$store` autosubscription — an F1
 * review rejection — and worse, its `derived` returns the *same mutated instance* every
 * time, so the `$state.raw` a rune bridge assigns into never changes identity and the list
 * silently stops re-rendering. Same vendor, same algorithm, one layer down: the adapter is
 * what is incompatible, not the choice.
 */
export class VirtualList {
	/** The visible slice, plus overscan. Read this in the template. */
	items = $state.raw<Pick<VirtualItem, 'index' | 'key' | 'start' | 'size'>[]>([]);
	/** The scroller's full scroll height in pixels. */
	total = $state(0);

	private v: Virtualizer<HTMLElement, HTMLElement>;
	private teardown: (() => void) | null = null;

	constructor(scroller: HTMLElement, count: number, estimateSize: number, overscan = 20) {
		this.v = new Virtualizer({
			count,
			estimateSize: () => estimateSize,
			overscan,
			getScrollElement: () => scroller,
			observeElementRect,
			observeElementOffset,
			scrollToFn: elementScroll,
			onChange: () => this.read()
		});
	}

	/** Starts observing. Returns the teardown, so an `$effect` can just return it. */
	mount(): () => void {
		this.teardown = this.v._didMount();
		this.v._willUpdate();
		this.read();
		return () => {
			this.teardown?.();
			this.teardown = null;
		};
	}

	/** Re-runs the layout after the row count changes. */
	setCount(count: number): void {
		this.v.setOptions({ ...this.v.options, count });
		this.v._willUpdate();
		this.read();
	}

	/** Measures a rendered row, so a line that wraps does not lie about its height. */
	measure = (node: HTMLElement): void => this.v.measureElement(node);

	scrollToIndex(index: number, align: 'start' | 'center' | 'end' | 'auto' = 'start'): void {
		this.v.scrollToIndex(index, { align });
	}

	scrollToEnd(): void {
		this.v.scrollToEnd();
	}

	/** Within `threshold` px of the bottom — what "follow the tail" is decided from. */
	isAtEnd(threshold = 32): boolean {
		return this.v.isAtEnd(threshold);
	}

	private read(): void {
		this.items = this.v.getVirtualItems().map((i) => ({
			index: i.index,
			key: i.key,
			start: i.start,
			size: i.size
		}));
		this.total = this.v.getTotalSize();
	}
}
