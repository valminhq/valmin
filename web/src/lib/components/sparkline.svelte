<script lang="ts">
	import { AreaChart } from 'layerchart';
	import type { Sample } from '$lib/state/stats.svelte';

	let {
		samples,
		pick,
		label,
		value,
		max
	}: {
		samples: Sample[];
		pick: (s: Sample) => number | null;
		label: string;
		/** The formatted current reading, or null for "not known". */
		value: string | null;
		/** Fixes the y domain where the metric has one — a percentage always does. */
		max?: number;
	} = $props();

	// `↯` Nulls are dropped, not zeroed (E10). A gap in a sparkline is honest; a dip to the
	// floor reads as the server having gone quiet, which is a different fact entirely.
	const data = $derived(
		samples
			.map((s) => ({ t: s.t, v: pick(s) }))
			.filter((p): p is { t: number; v: number } => p.v !== null)
	);
</script>

<div class="grid gap-1">
	<div class="flex items-baseline justify-between">
		<span class="text-xs text-muted-foreground">{label}</span>
		<span class="text-sm font-medium tabular-nums">{value ?? 'unknown'}</span>
	</div>
	<div class="h-10" aria-hidden="true">
		{#if data.length > 1}
			<AreaChart
				{data}
				x="t"
				y="v"
				yDomain={max === undefined ? undefined : [0, max]}
				axis={false}
				grid={false}
				rule={false}
				tooltipContext={false}
				padding={{ top: 2, bottom: 2 }}
			/>
		{/if}
	</div>
</div>
