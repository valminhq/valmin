<script lang="ts">
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { isTransient } from '#lib/api/instances.js';

	let {
		state,
		restartRequired = false,
		pendingRestart = false
	}: { state: string; restartRequired?: boolean; pendingRestart?: boolean } = $props();

	// The mapping is by state *class*, not by a list of Valheim states with meanings
	// attached (F2). `error` is a parking state with one way out (`12 §2.4`), so it is the
	// one that has to look different.
	const variant = $derived(
		state === 'error'
			? 'destructive'
			: state === 'running'
				? 'success'
				: isTransient(state)
					? 'warning'
					: 'secondary'
	);
	const label = $derived(state.replaceAll('_', ' '));
</script>

<span class="inline-flex flex-wrap items-center gap-2">
	<Badge {variant}
		>{label}{#if isTransient(state)}…{/if}</Badge
	>
	{#if restartRequired}
		<Badge
			variant="warning-outline"
			title="A launch setting or the password changed. The server runs with the old values until it restarts."
			>restart required</Badge
		>
	{:else if pendingRestart}
		<Badge
			variant="outline"
			title="Saved mod or configuration changes are waiting for the next start."
			>pending restart</Badge
		>
	{/if}
</span>
