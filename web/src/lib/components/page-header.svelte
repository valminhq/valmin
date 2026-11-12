<script lang="ts">
	import type { Snippet } from 'svelte';

	// level is 1 for a page of its own and 2 for a section under the server's name.
	let {
		title,
		level = 1,
		children
	}: { title: string; level?: 1 | 2; children?: Snippet } = $props();
</script>

<!--
	The title of a page, with an optional sentence of description as its children.
-->
<header class="grid gap-1">
	<svelte:element this={`h${level}`} class="text-2xl font-semibold tracking-tight">
		{title}
	</svelte:element>
	{#if children}
		<p class="max-w-3xl text-sm text-muted-foreground">{@render children()}</p>
	{/if}
</header>
