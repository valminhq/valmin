export function navigationMenu(node: HTMLDetailsElement) {
	node.dataset.navigationMenu = '';
	function outside(event: PointerEvent) {
		if (event.target instanceof Node && !node.contains(event.target)) node.open = false;
	}
	function escape(event: KeyboardEvent) {
		if (event.key !== 'Escape' || !node.open) return;
		event.preventDefault();
		node.open = false;
		node.querySelector('summary')?.focus();
	}
	function toggle() {
		if (!node.open) return;
		for (const other of document.querySelectorAll<HTMLDetailsElement>(
			'details[data-navigation-menu]'
		)) {
			if (other !== node) other.open = false;
		}
	}
	document.addEventListener('pointerdown', outside);
	document.addEventListener('keydown', escape);
	node.addEventListener('toggle', toggle);
	return {
		destroy() {
			document.removeEventListener('pointerdown', outside);
			document.removeEventListener('keydown', escape);
			node.removeEventListener('toggle', toggle);
		}
	};
}
