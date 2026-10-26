<script lang="ts">
	import type { PlayerName } from '#lib/api/players.js';

	let { names }: { names: PlayerName[] } = $props();

	const believed = $derived(names.filter((n) => n.likely));
	const maybe = $derived(names.filter((n) => !n.likely));
</script>

{#each believed as n (n.name)}
	<span class="text-sm">{n.name}</span>
{/each}
{#each maybe as n (n.name)}
	<span class="text-xs text-muted-foreground" title="Matched by timing, not read beside the ID"
		>maybe {n.name} · {n.hits}×</span
	>
{/each}
