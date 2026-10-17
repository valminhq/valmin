<script lang="ts">
	import type { AlertRule, Webhook } from '$lib/api/notifications';
	import {
		DEFAULT_CRASH_COUNT,
		DEFAULT_CRASH_WINDOW_MINUTES,
		DEFAULT_STALE_FACTOR,
		DEFAULT_STUCK_MINUTES,
		clock,
		eventOf
	} from '$lib/notification-events';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Switch } from '$lib/components/ui/switch';
	import Copy from '@lucide/svelte/icons/copy';
	import History from '@lucide/svelte/icons/history';
	import Pencil from '@lucide/svelte/icons/pencil';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	/** Alert rules as rows: the event, where it posts, its settings in brief, and its actions. A row
	 * without the change handlers is read-only. `channels` names the webhooks the rules post to,
	 * and `ondelete` runs once the deletion is confirmed. */
	let {
		rules,
		channels,
		busy = false,
		ontoggle,
		onedit,
		onduplicate,
		ondelete,
		ondeliveries
	}: {
		rules: AlertRule[];
		channels: Webhook[];
		busy?: boolean;
		ontoggle?: (rule: AlertRule, enabled: boolean) => void;
		onedit?: (rule: AlertRule) => void;
		onduplicate?: (rule: AlertRule) => void;
		ondelete?: (rule: AlertRule) => void;
		ondeliveries?: (rule: AlertRule) => void;
	} = $props();

	let deleting = $state<AlertRule | null>(null);
	let deleteOpen = $state(false);

	const channelNames = (r: AlertRule) =>
		r.webhook_ids.map((id) => channels.find((c) => c.id === id)?.name ?? 'a deleted channel');

	/** The rule's settings that differ by rule, as short phrases. */
	function details(r: AlertRule): string[] {
		const p = r.params;
		const out: string[] = [];
		if (r.condition_kind === 'crash_loop')
			out.push(
				`${p.crash_count || DEFAULT_CRASH_COUNT} stops in ${(p.crash_window_seconds ?? 0) / 60 || DEFAULT_CRASH_WINDOW_MINUTES} min`
			);
		if (r.condition_kind === 'job_stuck')
			out.push(`after ${(p.stuck_after_seconds ?? 0) / 60 || DEFAULT_STUCK_MINUTES} min`);
		if (r.condition_kind === 'stale_backup')
			out.push(`${p.stale_factor || DEFAULT_STALE_FACTOR}× the interval`);
		if (r.quiet_start_minutes !== null && r.quiet_end_minutes !== null && r.quiet_timezone)
			out.push(
				`quiet ${clock(r.quiet_start_minutes)}–${clock(r.quiet_end_minutes)} ${r.quiet_timezone}`
			);
		if (p.timezone) out.push(`times in ${p.timezone}`);
		return out;
	}
</script>

<ul class="divide-y rounded-lg border">
	{#each rules as r (r.id)}
		{@const e = eventOf(r.condition_kind)}
		{@const names = channelNames(r)}
		<li class="flex flex-wrap items-start gap-3 p-3" data-testid="rule-{r.id}">
			<Switch
				class="mt-0.5"
				checked={r.enabled}
				disabled={!ontoggle || busy}
				onCheckedChange={(on) => ontoggle?.(r, on)}
				aria-label="Send {e.label}"
			/>
			<div class="grid min-w-0 flex-1 gap-0.5">
				<div class="flex flex-wrap items-center gap-2">
					<span class="text-sm font-medium">{e.label}</span>
					{#if !r.enabled}<Badge variant="secondary">paused</Badge>{/if}
				</div>
				<p class="text-sm text-muted-foreground">
					{names.length > 0 ? `→ ${names.join(', ')}` : 'Posts nowhere yet'}
					{#each details(r) as detail (detail)}<span> · {detail}</span>{/each}
				</p>
				{#if r.params.template}
					<p class="truncate font-mono text-xs text-muted-foreground">
						{r.params.template.split('\n')[0]}
					</p>
				{/if}
			</div>
			<div class="flex flex-wrap items-center gap-1">
				{#if ondeliveries}
					<Button variant="ghost" size="sm" onclick={() => ondeliveries(r)}>
						<History /> Deliveries
					</Button>
				{/if}
				{#if onduplicate}
					<Button variant="ghost" size="sm" disabled={busy} onclick={() => onduplicate(r)}>
						<Copy /><span class="sr-only">Duplicate the {e.label} rule</span>
					</Button>
				{/if}
				{#if onedit}
					<Button variant="ghost" size="sm" disabled={busy} onclick={() => onedit(r)}>
						<Pencil /><span class="sr-only">Edit the {e.label} rule</span>
					</Button>
				{/if}
				{#if ondelete}
					<Button
						variant="ghost"
						size="sm"
						disabled={busy}
						onclick={() => {
							deleting = r;
							deleteOpen = true;
						}}
					>
						<Trash2 /><span class="sr-only">Delete the {e.label} rule</span>
					</Button>
				{/if}
			</div>
		</li>
	{/each}
</ul>

<Dialog.Root bind:open={deleteOpen}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Delete this rule?</Dialog.Title>
			<Dialog.Description>
				{deleting ? eventOf(deleting.condition_kind).label : 'Its'} messages stop. Its channels are kept.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (deleteOpen = false)}>Cancel</Button>
			<Button
				variant="destructive"
				onclick={() => {
					deleteOpen = false;
					if (deleting) ondelete?.(deleting);
				}}>Delete rule</Button
			>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
