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

<span class="inline-flex flex-wrap items-center gap-2">
	<Badge
		{variant}
		class={state === 'running'
			? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300'
			: isTransient(state)
				? 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300'
				: undefined}
		>{label}{#if isTransient(state)}…{/if}</Badge
	>
	{#if restartRequired}
		<Badge
			variant="outline"
			class="border-amber-300 text-amber-800 dark:border-amber-800 dark:text-amber-300"
			>restart required</Badge
		>
	{/if}
</span>
