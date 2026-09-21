/**
 * Dismissal for a `<details>` navigation menu: a click outside it, Escape, another menu
 * opening, or the control inside it that navigates.
 *
 * The element's own `open` is the whole state. A menu that also binds `open` to a component
 * variable has two owners: this closes the element, the variable stays true until the
 * asynchronous `toggle` event lands, and any render in between reopens the menu.
 */
export function navigationMenu(node: HTMLDetailsElement) {
	node.dataset.navigationMenu = '';
	function close() {
		node.open = false;
	}
	function outside(event: PointerEvent) {
		if (event.target instanceof Node && !node.contains(event.target)) close();
	}
	function escape(event: KeyboardEvent) {
		if (event.key !== 'Escape' || !node.open) return;
		event.preventDefault();
		close();
		node.querySelector('summary')?.focus();
	}
	// Every link and button in a menu navigates, and the menu must not cover where it lands.
	function inside(event: MouseEvent) {
		if (event.target instanceof Element && event.target.closest('a, button')) close();
	}
	function toggle() {
		if (!node.open) return;
		for (const other of document.querySelectorAll<HTMLDetailsElement>(
			'details[data-navigation-menu]'
		)) {
			if (other !== node) other.open = false;
		}
	}
	// Capture: a handler between the click and the document cannot hold a menu open by
	// stopping propagation.
	document.addEventListener('pointerdown', outside, true);
	document.addEventListener('keydown', escape, true);
	node.addEventListener('click', inside);
	node.addEventListener('toggle', toggle);
	return {
		destroy() {
			document.removeEventListener('pointerdown', outside, true);
			document.removeEventListener('keydown', escape, true);
			node.removeEventListener('click', inside);
			node.removeEventListener('toggle', toggle);
		}
	};
}
