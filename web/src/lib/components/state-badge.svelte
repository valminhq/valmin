<script lang="ts">
	import { Badge } from '$lib/components/ui/badge';
	import { isTransient } from '$lib/api/instances';

	let { state, restartRequired = false }: { state: string; restartRequired?: boolean } = $props();

	// The mapping is by state *class*, not by a list of Valheim states with meanings
	// attached (F2). `error` is a parking state with one way out (`12 §2.4`), so it is the
	// one that has to look different.
	const variant = $derived(
		state === 'error' ? 'destructive' : state === 'running' ? 'default' : 'secondary'
	);
	const label = $derived(state.replaceAll('_', ' '));
</script>

<span class="inline-flex items-center gap-2">
	<Badge {variant}
		>{label}{#if isTransient(state)}…{/if}</Badge
	>
	{#if restartRequired}
		<Badge variant="outline">restart required</Badge>
	{/if}
</span>
