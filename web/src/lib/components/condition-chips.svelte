<script lang="ts">
	import type { InboxItem } from '$lib/api/inbox';
	import { CONDITION_LABEL, conditionTitle } from '$lib/conditions';
	import { Badge } from '$lib/components/ui/badge';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	// Already filtered and ordered by chipsFor: this renders, it does not decide.
	let { items }: { items: InboxItem[] } = $props();
</script>

{#each items as item (item.kind)}
	<Badge
		variant={item.severity === 'critical' ? 'destructive' : 'outline'}
		class={item.severity === 'critical'
			? undefined
			: 'border-amber-300 text-amber-800 dark:border-amber-800 dark:text-amber-300'}
		title={conditionTitle(item)}
	>
		{#if item.severity === 'critical'}
			<TriangleAlert aria-hidden="true" />
		{/if}
		{CONDITION_LABEL[item.kind]}
	</Badge>
{/each}
