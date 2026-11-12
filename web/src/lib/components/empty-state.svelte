<script lang="ts">
	import type { Snippet } from 'svelte';

	let { title, children, actions }: { title?: string; children: Snippet; actions?: Snippet } =
		$props();
</script>

<!--
	The box shown where a list has nothing in it: an optional title, a sentence as its
	children, and the buttons that lead out of the empty state.
-->
<div class="grid justify-items-center gap-1 rounded-lg border border-dashed p-6 text-center">
	{#if title}<p class="font-medium">{title}</p>{/if}
	<p class="text-sm text-muted-foreground">{@render children()}</p>
	{#if actions}
		<div class="mt-2 flex flex-wrap justify-center gap-2">{@render actions()}</div>
	{/if}
</div>
