<script lang="ts">
	import { resolve } from '$app/paths';
	import type { InboxItem } from '$lib/api/inbox';
	import {
		CONDITION_LABEL,
		CONDITION_SENTENCE,
		conditionAge,
		conditionDetail
	} from '$lib/conditions';
	import { Badge } from '$lib/components/ui/badge';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	// Already filtered and ordered by chipsFor: this renders, it does not decide.
	let { items }: { items: InboxItem[] } = $props();

	/** Where a condition is acted on, for the kinds that have somewhere to go. The rest are
	 * read on the server's own page, which the card already links to. */
	function destination(item: InboxItem): { href: string; label: string } | null {
		if (!item.instance_id) return null;
		if (item.kind === 'stale_backup') {
			return {
				href: resolve('/instances/[id]/backups', { id: item.instance_id }),
				label: 'Open backups'
			};
		}
		if (item.kind === 'update_available') {
			return {
				href: resolve('/instances/[id]', { id: item.instance_id }),
				label: 'Open the server'
			};
		}
		return null;
	}
</script>

<!--
	The chips are the summary of a disclosure rather than bare badges: what each one means, its
	numbers and its age used to live only in a `title`, which a keyboard reaches never and a
	touch screen reaches by accident. One tab stop per card, not one per chip.
-->
{#if items.length > 0}
	<details class="min-w-0">
		<summary
			class="flex cursor-pointer flex-wrap items-center gap-2 rounded-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
		>
			{#each items as item (item.kind)}
				<Badge
					variant={item.severity === 'critical' ? 'destructive' : 'outline'}
					class={item.severity === 'critical'
						? undefined
						: 'border-amber-300 text-amber-800 dark:border-amber-800 dark:text-amber-300'}
				>
					{#if item.severity === 'critical'}
						<TriangleAlert aria-hidden="true" />
					{/if}
					{CONDITION_LABEL[item.kind]}
				</Badge>
			{/each}
			<span class="text-xs font-normal text-muted-foreground">
				{items.length === 1 ? 'What this means' : 'What these mean'}
			</span>
		</summary>
		<ul class="mt-2 grid gap-1.5 text-sm font-normal">
			{#each items as item (item.kind)}
				{@const detail = conditionDetail(item)}
				{@const link = destination(item)}
				<li class="grid gap-0.5">
					<!-- The separator is written out because the whitespace around a block tag is
					     trimmed, and a sentence running into its detail is what that produces. -->
					<span
						>{CONDITION_SENTENCE[item.kind]}{#if detail}{' · ' + detail}{/if}</span
					>
					<span class="text-xs text-muted-foreground">
						open {conditionAge(item)}
						{#if link}
							·
							<!-- Already resolved by destination(), which the rule cannot see through. -->
							<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
							<a class="underline" href={link.href}>{link.label}</a>
						{/if}
					</span>
				</li>
			{/each}
		</ul>
	</details>
{/if}
