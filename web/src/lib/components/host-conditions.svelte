<script lang="ts">
	import type { InboxItem } from '$lib/api/inbox';
	import { CONDITION_SENTENCE, conditionAge, conditionDetail } from '$lib/conditions';
	import * as Alert from '$lib/components/ui/alert';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	// Conditions with no server card to sit on: the host's own, and any whose server is not
	// on the list to carry it.
	let { items }: { items: InboxItem[] } = $props();

	const critical = $derived(items.some((item) => item.severity === 'critical'));
</script>

{#if items.length > 0}
	<Alert.Root variant={critical ? 'destructive' : 'default'}>
		<TriangleAlert />
		<Alert.Title>
			{items.length === 1
				? 'This host needs attention'
				: `This host needs attention (${items.length})`}
		</Alert.Title>
		<Alert.Description>
			<ul class="grid gap-1">
				{#each items as item (item.kind + (item.instance_id ?? ''))}
					{@const note = conditionDetail(item)}
					<li class="flex flex-wrap items-baseline gap-x-2">
						<span>
							{#if item.instance_name}<span class="font-medium">{item.instance_name}</span> ·
							{/if}{CONDITION_SENTENCE[item.kind]}{#if note}
								· {note}
							{/if}
						</span>
						<span class="text-xs opacity-70">{conditionAge(item)}</span>
					</li>
				{/each}
			</ul>
		</Alert.Description>
	</Alert.Root>
{/if}
