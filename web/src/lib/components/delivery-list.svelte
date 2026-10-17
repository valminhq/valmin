<script lang="ts">
	import type {
		AlertRule,
		Delivery,
		DeliveryFilter,
		DeliveryStatus,
		NotificationsApi,
		Webhook
	} from '$lib/api/notifications';
	import { formatInstant } from '$lib/api/schedules';
	import { GAME_EVENTS } from '$lib/game-events';
	import { eventOf } from '$lib/notification-events';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import { Label } from '$lib/components/ui/label';
	import * as Select from '$lib/components/ui/select';
	import Problem from '$lib/components/problem.svelte';
	import X from '@lucide/svelte/icons/x';

	/** The scope's delivery history, newest first, filtered by channel, status or the rule that
	 * sent it. `serverName`, when given, labels each delivery with its server. Call `reload`
	 * after a change that sends something. */
	let {
		api,
		channels,
		serverName,
		rule = $bindable(null)
	}: {
		api: NotificationsApi;
		channels: Webhook[];
		serverName?: (id: string) => string;
		rule?: AlertRule | null;
	} = $props();

	const ANY = 'any';
	const STATUSES: DeliveryStatus[] = ['pending', 'delivered', 'failed'];
	const STATUS_LABEL: Record<DeliveryStatus, string> = {
		pending: 'Pending',
		delivered: 'Delivered',
		failed: 'Failed'
	};
	// What each event kind reads as; an unknown kind falls back to its words.
	const EVENT_LABEL: Record<string, string> = {
		test: 'Test notification',
		instance_down: 'Server stopped unexpectedly',
		update_available: 'Server update available',
		backup_failed: 'Backup failed',
		alert_opened: 'Alert raised',
		alert_resolved: 'Alert cleared',
		instance_auto_stopped: 'Server stopped: no players',
		power_cut_soon: 'Power cut soon',
		server_started: 'Server started',
		server_stopped: 'Server stopped',
		...Object.fromEntries(GAME_EVENTS.map((e) => [e.kind, e.headline]))
	};

	let deliveries = $state.raw<Delivery[]>([]);
	let cursor = $state<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<unknown>(null);
	let channel = $state(ANY);
	let status = $state(ANY);

	const filter = $derived<DeliveryFilter>({
		webhook_id: channel === ANY ? undefined : channel,
		status: STATUSES.find((s) => s === status),
		rule_id: rule?.id ?? undefined
	});
	const filtered = $derived(Object.values(filter).some(Boolean));
	const named = (id: string) => channels.find((c) => c.id === id)?.name ?? 'a deleted channel';
	const label = (kind: string) => EVENT_LABEL[kind] ?? kind.replaceAll('_', ' ');

	// Re-runs whenever a filter changes, which also discards the old list's cursor.
	$effect(() => {
		void load(filter);
	});

	/** Loads the first page again, for the filters as they are. */
	export function reload() {
		void load(filter);
	}

	// A response is applied only if no newer load has started since its request left. Starting
	// one aborts the previous request, so a slow older answer cannot overwrite newer filters.
	let generation = 0;
	let controller = new AbortController();

	async function load(f: DeliveryFilter) {
		const mine = ++generation;
		controller.abort();
		controller = new AbortController();
		deliveries = [];
		cursor = null;
		loading = true;
		loadingMore = false;
		failure = null;
		try {
			const page = await api.webhooks.deliveries(f, undefined, controller.signal);
			if (mine !== generation) return;
			deliveries = page.items;
			cursor = page.next_cursor;
		} catch (err) {
			if (mine === generation) failure = err;
		} finally {
			if (mine === generation) loading = false;
		}
	}

	async function loadMore() {
		if (!cursor || loadingMore) return;
		const mine = generation;
		loadingMore = true;
		try {
			const page = await api.webhooks.deliveries(filter, cursor, controller.signal);
			if (mine !== generation) return;
			deliveries = [...deliveries, ...page.items];
			cursor = page.next_cursor;
		} catch (err) {
			if (mine === generation) failure = err;
		} finally {
			if (mine === generation) loadingMore = false;
		}
	}
</script>

<div class="grid gap-4">
	<div class="grid gap-3 sm:grid-cols-2">
		<div class="grid gap-2">
			<Label for="delivery-destination">Channel</Label>
			<Select.Root type="single" bind:value={channel}>
				<Select.Trigger id="delivery-destination">
					{channel === ANY ? 'Every channel' : named(channel)}
				</Select.Trigger>
				<Select.Content>
					<Select.Item value={ANY}>Every channel</Select.Item>
					{#each channels as c (c.id)}
						<Select.Item value={c.id}>{c.name}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
		<div class="grid gap-2">
			<Label for="delivery-status">Status</Label>
			<Select.Root type="single" bind:value={status}>
				<Select.Trigger id="delivery-status">
					{filter.status ? STATUS_LABEL[filter.status] : 'Any status'}
				</Select.Trigger>
				<Select.Content>
					<Select.Item value={ANY}>Any status</Select.Item>
					{#each STATUSES as s (s)}
						<Select.Item value={s}>{STATUS_LABEL[s]}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
	</div>

	{#if rule}
		<div
			class="flex flex-wrap items-center gap-2 rounded-lg border bg-muted/40 p-3"
			data-testid="delivery-rule-filter"
		>
			<p class="flex-1 text-sm">
				Only messages sent by the {eventOf(rule.condition_kind).label} rule. Messages sent before the
				panel began recording their rule are not listed.
			</p>
			<Button variant="ghost" size="sm" onclick={() => (rule = null)}>
				<X /> Clear rule filter
			</Button>
		</div>
	{/if}

	<Problem error={failure} />

	{#if deliveries.length > 0}
		<div class="grid gap-2">
			{#each deliveries as d (d.id)}
				<div class="grid gap-1 rounded-lg border p-3" data-testid="delivery-{d.id}">
					<div class="flex flex-wrap items-center gap-2">
						<span class="text-sm font-medium">{label(d.event_kind)}</span>
						<span class="text-sm text-muted-foreground">→ {named(d.webhook_id)}</span>
						{#if d.instance_id && serverName}
							<Badge variant="outline">{serverName(d.instance_id)}</Badge>
						{/if}
						<Badge
							variant={d.status === 'delivered'
								? 'outline'
								: d.status === 'failed'
									? 'destructive'
									: 'secondary'}
						>
							{STATUS_LABEL[d.status] ?? d.status}
						</Badge>
					</div>
					<p class="text-sm text-muted-foreground">
						{d.attempts} attempt{d.attempts === 1 ? '' : 's'} · created {formatInstant(
							d.created_at
						)}
						{#if d.updated_at !== d.created_at}· updated {formatInstant(d.updated_at)}{/if}
					</p>
					{#if d.status === 'failed' && d.last_error}
						<p class="text-xs text-destructive">{d.last_error}</p>
					{/if}
				</div>
			{/each}
		</div>
	{:else if !loading && !failure}
		<p class="text-sm text-muted-foreground">
			{filtered ? 'No deliveries match these filters.' : 'Nothing has been sent yet.'}
		</p>
	{/if}

	{#if loading || loadingMore}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if cursor}
		<Button variant="outline" class="justify-self-start" onclick={loadMore}>Load more</Button>
	{/if}
</div>
