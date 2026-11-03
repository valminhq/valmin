<script lang="ts">
	import { AreaChart } from 'layerchart';
	import type { Sample } from '#lib/state/stats.svelte.js';

	/** One line of the sparkline. With more than one, `label` names it in the legend. */
	interface Series {
		key: string;
		label: string;
		/** A CSS colour, such as `var(--color-chart-rx)`. */
		color: string;
		pick: (s: Sample) => number | null;
	}

	let {
		samples,
		series,
		label,
		value,
		max
	}: {
		samples: Sample[];
		series: Series[];
		label: string;
		/** The formatted current reading, or null for "not known". */
		value: string | null;
		/** Fixes the y domain where the metric has one — a percentage always does. */
		max?: number;
	} = $props();

	// Nulls are dropped, not zeroed (E10). A gap in a sparkline is honest; a dip to the
	// floor reads as the server having gone quiet, which is a different fact entirely.
	const data = $derived(
		samples.flatMap((s) => {
			const row: Record<string, number> = { t: s.t };
			for (const line of series) {
				const v = line.pick(s);
				if (v === null) return [];
				row[line.key] = v;
			}
			return [row];
		})
	);
	const lines = $derived(series.map(({ key, label, color }) => ({ key, label, color })));
	// An all-zero window would leave an empty domain, which d3 draws at mid-height.
	const domain = $derived.by((): [number, number] | undefined => {
		if (max !== undefined) return [0, max];
		const top = Math.max(0, ...data.flatMap((row) => series.map((line) => row[line.key])));
		return top === 0 ? [0, 1] : undefined;
	});
</script>

<div class="grid gap-1">
	<div class="flex flex-wrap items-baseline justify-between gap-x-3">
		<span class="text-xs text-muted-foreground">{label}</span>
		<span class="text-xl font-semibold tracking-tight tabular-nums">{value ?? 'unknown'}</span>
	</div>
	<div class="h-10" aria-hidden="true">
		{#if data.length > 1}
			<AreaChart
				{data}
				x="t"
				series={lines}
				seriesLayout="overlap"
				yDomain={domain}
				axis={false}
				grid={false}
				rule={false}
				tooltipContext={false}
				padding={{ top: 2, bottom: 2 }}
			/>
		{/if}
	</div>
	{#if series.length > 1}
		<ul class="flex flex-wrap gap-x-3 text-xs text-muted-foreground">
			{#each series as line (line.key)}
				<li class="flex items-center gap-1.5">
					<span class="size-2 rounded-full" style:background-color={line.color}></span>
					{line.label}
				</li>
			{/each}
		</ul>
	{/if}
</div>
