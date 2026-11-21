<script lang="ts">
	import type { Snippet } from 'svelte';
	import { Button } from '#lib/components/ui/button/index.js';

	// The bar a full-page editor pins to the bottom of the screen while it holds unsaved edits:
	// a summary of what changed, Discard, and the save action.
	let {
		saveLabel = 'Save changes',
		busy = false,
		disabled = false,
		ondiscard,
		onsave,
		children
	}: {
		saveLabel?: string;
		busy?: boolean;
		disabled?: boolean;
		ondiscard: () => void;
		onsave: () => void;
		children: Snippet;
	} = $props();
</script>

<div
	class="sticky bottom-0 -mx-4 flex flex-wrap items-center justify-between gap-3 border-t bg-background/95 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6"
>
	<p class="text-sm text-muted-foreground">{@render children()}</p>
	<div class="flex gap-2">
		<Button variant="ghost" size="sm" disabled={busy} onclick={ondiscard}>Discard changes</Button>
		<Button size="sm" disabled={disabled || busy} onclick={onsave}>
			{busy ? 'Saving…' : saveLabel}
		</Button>
	</div>
</div>
