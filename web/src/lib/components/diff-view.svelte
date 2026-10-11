<script lang="ts">
	import type { DiffLine } from '$lib/diff';

	/** A line diff as `hunks()` groups it, with the old and new line numbers beside each line. */
	let { groups }: { groups: DiffLine[][] } = $props();
</script>

<div class="max-h-96 overflow-auto rounded-md border font-mono text-xs">
	{#each groups as group, i (i)}
		{#if i > 0}
			<div class="border-y bg-muted px-3 py-1 text-muted-foreground">⋯</div>
		{/if}
		{#each group as line (`${line.before}:${line.after}`)}
			<div
				class="flex gap-3 px-3 py-0.5 {line.kind === 'added'
					? 'bg-primary/10'
					: line.kind === 'removed'
						? 'bg-destructive/10'
						: ''}"
			>
				<span class="w-10 shrink-0 text-right text-muted-foreground tabular-nums select-none">
					{line.before || ''}
				</span>
				<span class="w-10 shrink-0 text-right text-muted-foreground tabular-nums select-none">
					{line.after || ''}
				</span>
				<span class="w-3 shrink-0 select-none">
					{line.kind === 'added' ? '+' : line.kind === 'removed' ? '−' : ''}
				</span>
				<span class="break-all whitespace-pre-wrap">{line.text}</span>
			</div>
		{/each}
	{/each}
</div>
