<script lang="ts">
	import { tick } from 'svelte';
	import { resolve } from '$app/paths';
	import {
		alertRuleAdmin,
		webhookAdmin,
		type AlertRule,
		type CreateAlertRule,
		type Delivery,
		type DeliveryFilter,
		type DeliveryStatus,
		type Webhook,
		type CreateWebhook
	} from '$lib/api/admin';
	import type { InboxKind } from '$lib/api/inbox';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { CONDITION_LABEL } from '$lib/conditions';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import History from '@lucide/svelte/icons/history';
	import Pencil from '@lucide/svelte/icons/pencil';
	import Send from '@lucide/svelte/icons/send';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import X from '@lucide/svelte/icons/x';

	const EVERY = 'every';
	const kinds = Object.keys(CONDITION_LABEL) as InboxKind[];
	// The kinds whose thresholds a rule can set, as alert_scan resolves them.
	const TUNABLE: InboxKind[] = ['crash_loop', 'job_stuck', 'stale_backup'];
	const STATUSES: DeliveryStatus[] = ['pending', 'delivered', 'failed'];
	const STATUS_LABEL: Record<DeliveryStatus, string> = {
		pending: 'Pending',
		delivered: 'Delivered',
		failed: 'Failed'
	};
	// What each event kind reads as in the delivery list; an unknown kind falls back to its words.
	const EVENT_LABEL: Record<string, string> = {
		test: 'Test notification',
		instance_down: 'Server stopped unexpectedly',
		update_available: 'Server update available',
		backup_failed: 'Backup failed',
		alert_opened: 'Alert raised',
		alert_resolved: 'Alert cleared'
	};

	let destinations = $state<Webhook[]>([]);
	let deliveries = $state.raw<Delivery[]>([]);
	let deliveryCursor = $state<string | null>(null);
	let deliveriesLoading = $state(true);
	let loadingMore = $state(false);
	let deliveryFailure = $state<unknown>(null);
	let filterDestination = $state(EVERY);
	let filterStatus = $state(EVERY);
	// The rule whose deliveries are shown, kept whole so its label outlives a reload.
	let filterRule = $state<AlertRule | null>(null);
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let name = $state('');
	let kind = $state<CreateWebhook['kind']>('discord');
	let url = $state('');
	let testJob = $state<string | null>(null);
	let deleting = $state<Webhook | null>(null);
	let deleteOpen = $state(false);
	let rules = $state<AlertRule[]>([]);
	let servers = $state<Instance[]>([]);
	let ruleKind = $state<InboxKind>('crash_loop');
	let ruleServer = $state(EVERY);
	let ruleDestinations = $state<string[]>([]);
	let deletingRule = $state<AlertRule | null>(null);
	let ruleDeleteOpen = $state(false);
	let editing = $state<AlertRule | null>(null);
	// Thresholds as typed: null is an empty field, which means the default.
	let crashCount = $state<number | null>(null);
	let crashWindowMinutes = $state<number | null>(null);
	let stuckMinutes = $state<number | null>(null);
	let staleFactor = $state<number | null>(null);
	let quietOn = $state(false);
	let quietStart = $state('22:00');
	let quietEnd = $state('07:00');
	const localZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
	let quietZone = $state(localZone);
	const zones =
		typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [];

	// Rendered from allowed_actions, never from a role name (F3). The daemon answers 404 to a
	// caller without it, so hiding the page's controls matches what the endpoints report.
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));
	const ready = $derived(name.trim() !== '' && url.trim() !== '' && !saving);
	// Low disk is host-wide: a rule for it always covers every server.
	const ruleHostWide = $derived(ruleKind === 'low_disk');
	const ruleScope = $derived(ruleHostWide ? EVERY : ruleServer);
	// Ticked destinations that still exist; a deleted one drops out.
	const ruleTicked = $derived(
		ruleDestinations.filter((id) => destinations.some((w) => w.id === id))
	);
	// Mirrors the daemon: counts and durations are 0 or more, a stale factor 0 or above 1.
	const badCount = $derived(
		ruleKind === 'crash_loop' &&
			crashCount !== null &&
			!(Number.isInteger(crashCount) && crashCount >= 0)
	);
	const badWindow = $derived(
		ruleKind === 'crash_loop' && crashWindowMinutes !== null && !(crashWindowMinutes >= 0)
	);
	const badStuck = $derived(
		ruleKind === 'job_stuck' && stuckMinutes !== null && !(stuckMinutes >= 0)
	);
	const badFactor = $derived(
		ruleKind === 'stale_backup' && staleFactor !== null && staleFactor !== 0 && !(staleFactor > 1)
	);
	// The daemon reads a window whose start equals its end as empty.
	const badQuiet = $derived(
		quietOn &&
			(quietStart === '' || quietEnd === '' || quietZone.trim() === '' || quietStart === quietEnd)
	);
	const ruleValid = $derived(!badCount && !badWindow && !badStuck && !badFactor && !badQuiet);
	const ruleReady = $derived(ruleTicked.length > 0 && ruleValid && !saving);

	/** The chosen kind's thresholds on the wire. An empty field is left out, so it reads as
	 * the default. */
	const ruleParams = $derived.by(() => {
		const p: AlertRule['params'] = {};
		const seconds = (minutes: number | null) =>
			minutes === null ? undefined : Math.round(minutes * 60);
		if (ruleKind === 'crash_loop') {
			if (crashCount !== null) p.crash_count = crashCount;
			if (crashWindowMinutes !== null) p.crash_window_seconds = seconds(crashWindowMinutes);
		} else if (ruleKind === 'job_stuck') {
			if (stuckMinutes !== null) p.stuck_after_seconds = seconds(stuckMinutes);
		} else if (ruleKind === 'stale_backup') {
			if (staleFactor !== null) p.stale_factor = staleFactor;
		}
		return p;
	});

	/** Every editable field, for both create and update. Quiet hours off sends an empty
	 * timezone, which clears a stored window. */
	const ruleBody = $derived<CreateAlertRule>({
		condition_kind: ruleKind,
		instance_id: ruleScope === EVERY ? null : ruleScope,
		webhook_ids: ruleTicked,
		params: ruleParams,
		quiet_start_minutes: quietOn ? minutesOf(quietStart) : 0,
		quiet_end_minutes: quietOn ? minutesOf(quietEnd) : 0,
		quiet_timezone: quietOn ? quietZone.trim() : ''
	});

	const deliveryFilter = $derived<DeliveryFilter>({
		webhook_id: filterDestination === EVERY ? undefined : filterDestination,
		status: STATUSES.find((s) => s === filterStatus),
		rule_id: filterRule?.id
	});
	const filtered = $derived(Object.values(deliveryFilter).some(Boolean));

	$effect(() => {
		void load();
	});

	// Re-runs whenever a filter changes, which also discards the old list's cursor.
	$effect(() => {
		void reloadDeliveries(deliveryFilter);
	});

	async function load() {
		try {
			[destinations, rules, servers] = await Promise.all([
				webhookAdmin.list(),
				alertRuleAdmin.list(),
				instances.list()
			]);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	// A response is applied only if no newer load has started since its request left. Starting
	// one aborts the previous request, so a slow older answer cannot overwrite newer filters.
	let generation = 0;
	let controller = new AbortController();

	async function reloadDeliveries(filter: DeliveryFilter) {
		const mine = ++generation;
		controller.abort();
		controller = new AbortController();
		deliveries = [];
		deliveryCursor = null;
		deliveriesLoading = true;
		loadingMore = false;
		deliveryFailure = null;
		try {
			const page = await webhookAdmin.deliveries(filter, undefined, controller.signal);
			if (mine !== generation) return;
			deliveries = page.items;
			deliveryCursor = page.next_cursor;
		} catch (err) {
			if (mine === generation) deliveryFailure = err;
		} finally {
			if (mine === generation) deliveriesLoading = false;
		}
	}

	async function loadMore() {
		if (!deliveryCursor || loadingMore) return;
		const mine = generation;
		loadingMore = true;
		try {
			const page = await webhookAdmin.deliveries(deliveryFilter, deliveryCursor, controller.signal);
			if (mine !== generation) return;
			deliveries = [...deliveries, ...page.items];
			deliveryCursor = page.next_cursor;
		} catch (err) {
			if (mine === generation) deliveryFailure = err;
		} finally {
			if (mine === generation) loadingMore = false;
		}
	}

	function refresh() {
		void load();
		void reloadDeliveries(deliveryFilter);
	}

	/** Narrows the delivery list to what one rule sent and brings the list into view. */
	function showDeliveries(rule: AlertRule) {
		filterRule = rule;
		filterDestination = EVERY;
		void tick().then(() =>
			document.getElementById('deliveries')?.scrollIntoView({ block: 'start', behavior: 'smooth' })
		);
	}

	async function act(call: () => Promise<unknown>) {
		saving = true;
		failure = null;
		try {
			await call();
			await load();
			void reloadDeliveries(deliveryFilter);
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	function add() {
		void act(async () => {
			await webhookAdmin.create({ name: name.trim(), kind, url: url.trim() });
			name = '';
			url = '';
		});
	}

	const toggle = (w: Webhook, enabled: boolean) =>
		void act(() => webhookAdmin.update(w.id, { enabled }));

	function test(w: Webhook) {
		void act(async () => {
			testJob = (await webhookAdmin.test(w.id)).job_id;
		});
	}

	function remove() {
		const target = deleting;
		if (!target) return;
		void act(async () => {
			await webhookAdmin.remove(target.id);
			if (filterDestination === target.id) filterDestination = EVERY;
		});
	}

	function ask(w: Webhook) {
		deleting = w;
		deleteOpen = true;
	}

	/** Loads a rule into the form for editing, or clears the form for a new rule. */
	function fill(rule: AlertRule | null) {
		const p = rule?.params ?? {};
		const minutes = (seconds?: number) => (seconds === undefined ? null : seconds / 60);
		editing = rule;
		ruleKind = rule?.condition_kind ?? 'crash_loop';
		ruleServer = rule?.instance_id ?? EVERY;
		ruleDestinations = rule ? [...rule.webhook_ids] : [];
		crashCount = p.crash_count ?? null;
		crashWindowMinutes = minutes(p.crash_window_seconds);
		stuckMinutes = minutes(p.stuck_after_seconds);
		staleFactor = p.stale_factor ?? null;
		quietOn = !!rule?.quiet_timezone;
		quietStart = clock(rule?.quiet_start_minutes ?? 22 * 60);
		quietEnd = clock(rule?.quiet_end_minutes ?? 7 * 60);
		quietZone = rule?.quiet_timezone ?? localZone;
	}

	function edit(rule: AlertRule) {
		fill(rule);
		void tick().then(() => document.getElementById('rule-condition')?.focus());
	}

	function saveRule() {
		const target = editing;
		const body = ruleBody;
		void act(async () => {
			// The daemon ignores a null instance_id on a patch; an empty one clears the server.
			if (target)
				await alertRuleAdmin.update(target.id, { ...body, instance_id: body.instance_id ?? '' });
			else await alertRuleAdmin.create(body);
			fill(null);
		});
	}

	const toggleRule = (rule: AlertRule, enabled: boolean) =>
		void act(() => alertRuleAdmin.update(rule.id, { enabled }));

	function askRule(rule: AlertRule) {
		deletingRule = rule;
		ruleDeleteOpen = true;
	}

	function removeRule() {
		const target = deletingRule;
		ruleDeleteOpen = false;
		if (!target) return;
		void act(async () => {
			await alertRuleAdmin.remove(target.id);
			if (editing?.id === target.id) fill(null);
			if (filterRule?.id === target.id) filterRule = null;
		});
	}

	function pick(id: string, on: boolean) {
		ruleDestinations = on
			? [...ruleDestinations, id]
			: ruleDestinations.filter((other) => other !== id);
	}

	const condition = (kind: InboxKind) => {
		const label = CONDITION_LABEL[kind] ?? kind;
		return label.charAt(0).toUpperCase() + label.slice(1);
	};
	const serverName = (id: string | null) =>
		id === null ? 'Every server' : (servers.find((s) => s.id === id)?.name ?? 'a deleted server');
	const named = (id: string) =>
		destinations.find((w) => w.id === id)?.name ?? 'a deleted destination';
	/** Minutes from midnight as the HH:MM a time input holds, and back. */
	function clock(minutes: number) {
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${pad(Math.floor(minutes / 60))}:${pad(minutes % 60)}`;
	}
	function minutesOf(hhmm: string) {
		return Number(hhmm.slice(0, 2)) * 60 + Number(hhmm.slice(3, 5));
	}

	// alerts.Params.Defaults: what an empty threshold means.
	const DEFAULT_CRASH_COUNT = 3;
	const DEFAULT_CRASH_WINDOW_MINUTES = 30;
	const DEFAULT_STUCK_MINUTES = 60;
	const DEFAULT_STALE_FACTOR = 2;

	/** The thresholds a rule runs with, defaults filled in. Empty for a kind that has none. */
	function thresholdText(rule: AlertRule) {
		const p = rule.params;
		const min = (seconds: number | undefined, fallback: number) =>
			seconds ? Number((seconds / 60).toFixed(1)) : fallback;
		if (rule.condition_kind === 'crash_loop') {
			const count = p.crash_count || DEFAULT_CRASH_COUNT;
			return `${count} stops in ${min(p.crash_window_seconds, DEFAULT_CRASH_WINDOW_MINUTES)} min`;
		}
		if (rule.condition_kind === 'job_stuck') {
			return `after ${min(p.stuck_after_seconds, DEFAULT_STUCK_MINUTES)} min`;
		}
		if (rule.condition_kind === 'stale_backup') {
			const factor = p.stale_factor && p.stale_factor > 1 ? p.stale_factor : DEFAULT_STALE_FACTOR;
			return `${factor}× the backup interval`;
		}
		return '';
	}

	function quietText(rule: AlertRule) {
		const { quiet_start_minutes: start, quiet_end_minutes: end, quiet_timezone: zone } = rule;
		if (start === null || end === null || !zone) return '';
		return `Quiet ${clock(start)}–${clock(end)}, ${zone}`;
	}

	const event = (kind: string) => EVENT_LABEL[kind] ?? kind.replaceAll('_', ' ');
	const when = (iso: string) => new Date(iso).toLocaleString();
</script>

<main class="mx-auto grid max-w-3xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Notifications</h1>
		<p class="text-sm text-muted-foreground">
			The panel posts to these when a server stops on its own, a game update appears, or a backup
			fails. A stop you asked for is not a notification. Alert rules send chosen conditions to
			chosen destinations. When a rule already sends the same incident to a destination, that
			destination gets only the rule's alert.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">Notification settings are not available to you.</p>
	{:else}
		<Card.Root>
			<Card.Header>
				<Card.Title>Destinations</Card.Title>
				<Card.Description>
					A Discord webhook URL is a password: anyone holding it can post to that channel. The panel
					stores it encrypted and never shows it again, so keep your own copy if you need one.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				{#if loading}
					<p class="text-sm text-muted-foreground">Loading…</p>
				{:else if destinations.length === 0}
					<p class="text-sm text-muted-foreground">
						No notification destinations yet. Add one below, then send a test notification.
					</p>
				{:else}
					<div class="grid gap-2">
						{#each destinations as w (w.id)}
							<div class="flex flex-wrap items-center gap-3 rounded-lg border p-3">
								<div class="grid flex-1 gap-1">
									<div class="flex flex-wrap items-center gap-2">
										<span class="text-sm font-medium">{w.name}</span>
										<Badge variant="outline">{w.kind}</Badge>
										{#if !w.enabled}<Badge variant="secondary">paused</Badge>{/if}
									</div>
									<p class="text-sm text-muted-foreground">added {when(w.created_at)}</p>
								</div>
								<Button variant="outline" size="sm" disabled={saving} onclick={() => test(w)}>
									<Send /> Send test notification
								</Button>
								<Switch
									checked={w.enabled}
									disabled={saving}
									onCheckedChange={(v) => toggle(w, v)}
									aria-label="Enable notifications for {w.name}"
								/>
								<Button variant="ghost" size="sm" disabled={saving} onclick={() => ask(w)}>
									<Trash2 />
									<span class="sr-only">Delete destination</span>
								</Button>
							</div>
						{/each}
					</div>
				{/if}

				{#if testJob}
					<div class="rounded-lg border p-4">
						<JobProgress jobId={testJob} onfinish={refresh} />
					</div>
				{/if}

				<div class="grid gap-3 border-t pt-4">
					<div class="grid gap-3 sm:grid-cols-[1fr_10rem]">
						<div class="grid gap-2">
							<Label for="webhook-name">Name</Label>
							<Input id="webhook-name" bind:value={name} placeholder="Ops channel" />
						</div>
						<div class="grid gap-2">
							<Label for="webhook-kind">Receiver</Label>
							<Select.Root type="single" bind:value={kind}>
								<Select.Trigger id="webhook-kind">
									{kind === 'discord' ? 'Discord' : 'Generic JSON'}
								</Select.Trigger>
								<Select.Content>
									<Select.Item value="discord">Discord</Select.Item>
									<Select.Item value="generic">Generic JSON</Select.Item>
								</Select.Content>
							</Select.Root>
						</div>
					</div>
					<div class="grid gap-2">
						<Label for="webhook-url">URL</Label>
						<Input
							id="webhook-url"
							bind:value={url}
							placeholder="https://discord.com/api/webhooks/…"
						/>
						<!--
							ADR-167: the address policy is enforced at the daemon and reported on this field,
							so the rule an operator will trip over is stated before they trip over it.
						-->
						<p class="text-sm text-muted-foreground">
							Must be an <code>https://</code> address on the public internet. Addresses on this machine
							or its network are refused, and the panel does not follow redirects.
						</p>
					</div>
					<Button class="justify-self-start" disabled={!ready} onclick={add}>Add destination</Button
					>
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Alert rules</Card.Title>
				<Card.Description>
					Each rule sends one condition to the destinations it names, when the condition opens and
					when it clears. A rule can also set the condition's thresholds and quiet hours.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				{#if !loading && rules.length === 0}
					<p class="text-sm text-muted-foreground">No alert rules yet.</p>
				{:else if rules.length > 0}
					<div class="grid gap-2">
						{#each rules as rule (rule.id)}
							<div
								class="flex flex-wrap items-center gap-3 rounded-lg border p-3"
								data-testid="alert-rule-{rule.id}"
							>
								<div class="grid flex-1 gap-1">
									<div class="flex flex-wrap items-center gap-2">
										<span class="text-sm font-medium">{condition(rule.condition_kind)}</span>
										<Badge variant="outline">{serverName(rule.instance_id)}</Badge>
										{#if !rule.enabled}<Badge variant="secondary">paused</Badge>{/if}
									</div>
									{#if thresholdText(rule) || quietText(rule)}
										<p class="text-sm text-muted-foreground">
											{[thresholdText(rule), quietText(rule)].filter(Boolean).join(' · ')}
										</p>
									{/if}
									{#if rule.webhook_ids.length === 0}
										<p class="text-sm text-muted-foreground">
											No destinations, so this rule sends nothing.
										</p>
									{:else}
										<p class="text-sm text-muted-foreground">
											→ {rule.webhook_ids.map(named).join(', ')}
										</p>
									{/if}
								</div>
								<Button variant="outline" size="sm" onclick={() => showDeliveries(rule)}>
									<History /> Show deliveries
								</Button>
								<Switch
									checked={rule.enabled}
									disabled={saving}
									onCheckedChange={(v) => toggleRule(rule, v)}
									aria-label="Enable the {condition(rule.condition_kind)} rule"
								/>
								<Button variant="ghost" size="sm" disabled={saving} onclick={() => edit(rule)}>
									<Pencil />
									<span class="sr-only">Edit the {condition(rule.condition_kind)} rule</span>
								</Button>
								<Button variant="ghost" size="sm" disabled={saving} onclick={() => askRule(rule)}>
									<Trash2 />
									<span class="sr-only">Delete the {condition(rule.condition_kind)} rule</span>
								</Button>
							</div>
						{/each}
					</div>
				{/if}

				<div class="grid gap-3 border-t pt-4">
					{#if destinations.length === 0}
						<p class="text-sm text-muted-foreground">
							Add a destination above before adding a rule.
						</p>
					{:else}
						<h3 class="text-sm font-medium">{editing ? 'Edit rule' : 'New rule'}</h3>
						<div class="grid gap-3 sm:grid-cols-2">
							<div class="grid gap-2">
								<Label for="rule-condition">Condition</Label>
								<Select.Root type="single" bind:value={ruleKind}>
									<Select.Trigger id="rule-condition">{condition(ruleKind)}</Select.Trigger>
									<Select.Content>
										{#each kinds as k (k)}
											<Select.Item value={k}>{condition(k)}</Select.Item>
										{/each}
									</Select.Content>
								</Select.Root>
							</div>
							<div class="grid gap-2">
								<Label for="rule-server">Server</Label>
								<Select.Root type="single" bind:value={ruleServer} disabled={ruleHostWide}>
									<Select.Trigger id="rule-server">
										{ruleScope === EVERY ? 'Every server' : serverName(ruleScope)}
									</Select.Trigger>
									<Select.Content>
										<Select.Item value={EVERY}>Every server</Select.Item>
										{#each servers as s (s.id)}
											<Select.Item value={s.id}>{s.name}</Select.Item>
										{/each}
									</Select.Content>
								</Select.Root>
							</div>
						</div>
						<fieldset class="grid gap-2">
							<legend class="mb-2 text-sm font-medium">Send to</legend>
							{#each destinations as w (w.id)}
								<label class="flex items-center gap-2 text-sm">
									<input
										type="checkbox"
										checked={ruleDestinations.includes(w.id)}
										onchange={(e) => pick(w.id, e.currentTarget.checked)}
									/>
									{w.name}
								</label>
							{/each}
						</fieldset>
						{#if ruleKind === 'crash_loop'}
							<div class="grid gap-3 sm:grid-cols-2">
								<div class="grid gap-2">
									<Label for="rule-crash-count">Stops</Label>
									<Input
										id="rule-crash-count"
										type="number"
										min="0"
										step="1"
										placeholder={String(DEFAULT_CRASH_COUNT)}
										aria-invalid={badCount}
										bind:value={crashCount}
									/>
									{#if badCount}
										<p class="text-xs text-destructive">Enter a whole number, 0 or more.</p>
									{/if}
								</div>
								<div class="grid gap-2">
									<Label for="rule-crash-window">Within minutes</Label>
									<Input
										id="rule-crash-window"
										type="number"
										min="0"
										placeholder={String(DEFAULT_CRASH_WINDOW_MINUTES)}
										aria-invalid={badWindow}
										bind:value={crashWindowMinutes}
									/>
									{#if badWindow}
										<p class="text-xs text-destructive">Enter 0 or more minutes.</p>
									{/if}
								</div>
							</div>
						{:else if ruleKind === 'job_stuck'}
							<div class="grid gap-2 sm:w-1/2">
								<Label for="rule-stuck">Minutes a job may run</Label>
								<Input
									id="rule-stuck"
									type="number"
									min="0"
									placeholder={String(DEFAULT_STUCK_MINUTES)}
									aria-invalid={badStuck}
									bind:value={stuckMinutes}
								/>
								{#if badStuck}
									<p class="text-xs text-destructive">Enter 0 or more minutes.</p>
								{/if}
							</div>
						{:else if ruleKind === 'stale_backup'}
							<div class="grid gap-2 sm:w-1/2">
								<Label for="rule-stale">Times the backup interval</Label>
								<Input
									id="rule-stale"
									type="number"
									step="0.1"
									placeholder={String(DEFAULT_STALE_FACTOR)}
									aria-invalid={badFactor}
									bind:value={staleFactor}
								/>
								{#if badFactor}
									<p class="text-xs text-destructive">Enter a number above 1.</p>
								{/if}
							</div>
						{/if}
						{#if TUNABLE.includes(ruleKind)}
							<p class="text-sm text-muted-foreground">Leave a field empty to use the default.</p>
						{/if}

						<div class="flex items-center gap-2">
							<Switch id="rule-quiet" bind:checked={quietOn} />
							<Label for="rule-quiet">Quiet hours</Label>
						</div>
						{#if quietOn}
							<div class="grid gap-3 sm:grid-cols-[8rem_8rem_1fr]">
								<div class="grid gap-2">
									<Label for="rule-quiet-start">From</Label>
									<Input id="rule-quiet-start" type="time" step="60" bind:value={quietStart} />
								</div>
								<div class="grid gap-2">
									<Label for="rule-quiet-end">Until</Label>
									<Input id="rule-quiet-end" type="time" step="60" bind:value={quietEnd} />
								</div>
								<div class="grid gap-2">
									<Label for="rule-quiet-zone">Timezone</Label>
									<Input
										id="rule-quiet-zone"
										list="rule-zones"
										aria-invalid={quietZone.trim() === ''}
										bind:value={quietZone}
									/>
									<datalist id="rule-zones">
										{#each zones as zone (zone)}<option value={zone}></option>{/each}
									</datalist>
								</div>
							</div>
							{#if badQuiet}
								<p class="text-xs text-destructive">
									Set a start, an end and a timezone; start and end must differ.
								</p>
							{/if}
							<p class="text-sm text-muted-foreground">
								Alerts still open when quiet hours end are sent then; one that opens and clears
								inside the window is not sent.
							</p>
						{/if}

						<div class="flex flex-wrap gap-2">
							<Button disabled={!ruleReady} onclick={saveRule}>
								{editing ? 'Save rule' : 'Add rule'}
							</Button>
							{#if editing}
								<Button variant="outline" disabled={saving} onclick={() => fill(null)}>
									Cancel
								</Button>
							{/if}
						</div>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root id="deliveries">
			<Card.Header>
				<Card.Title>Recent deliveries</Card.Title>
				<Card.Description>
					Every attempt is recorded, including the ones that never arrived. A send is retried three
					times over two minutes and then left here as failed.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<div class="grid gap-3 sm:grid-cols-2">
					<div class="grid gap-2">
						<Label for="delivery-destination">Destination</Label>
						<Select.Root type="single" bind:value={filterDestination}>
							<Select.Trigger id="delivery-destination">
								{filterDestination === EVERY ? 'Every destination' : named(filterDestination)}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value={EVERY}>Every destination</Select.Item>
								{#each destinations as w (w.id)}
									<Select.Item value={w.id}>{w.name}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					</div>
					<div class="grid gap-2">
						<Label for="delivery-status">Status</Label>
						<Select.Root type="single" bind:value={filterStatus}>
							<Select.Trigger id="delivery-status">
								{deliveryFilter.status ? STATUS_LABEL[deliveryFilter.status] : 'Any status'}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value={EVERY}>Any status</Select.Item>
								{#each STATUSES as s (s)}
									<Select.Item value={s}>{STATUS_LABEL[s]}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					</div>
				</div>

				{#if filterRule}
					<div
						class="flex flex-wrap items-center gap-2 rounded-lg border bg-muted/40 p-3"
						data-testid="delivery-rule-filter"
					>
						<p class="flex-1 text-sm">
							Only alerts sent by the {condition(filterRule.condition_kind)} rule for
							{filterRule.instance_id ? serverName(filterRule.instance_id) : 'every server'}. Alerts
							sent before the panel began recording their rule are not listed.
						</p>
						<Button variant="ghost" size="sm" onclick={() => (filterRule = null)}>
							<X /> Clear rule filter
						</Button>
					</div>
				{/if}

				<Problem error={deliveryFailure} />

				{#if deliveries.length > 0}
					<div class="grid gap-2">
						{#each deliveries as d (d.id)}
							<div class="grid gap-1 rounded-lg border p-3" data-testid="delivery-{d.id}">
								<div class="flex flex-wrap items-center gap-2">
									<span class="text-sm font-medium">{event(d.event_kind)}</span>
									<span class="text-sm text-muted-foreground">→ {named(d.webhook_id)}</span>
									{#if d.instance_id}
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
									{d.attempts} attempt{d.attempts === 1 ? '' : 's'} · created {when(d.created_at)}
									{#if d.updated_at !== d.created_at}· updated {when(d.updated_at)}{/if}
								</p>
								{#if d.status === 'failed' && d.last_error}
									<p class="text-xs text-destructive">{d.last_error}</p>
								{/if}
							</div>
						{/each}
					</div>
				{:else if !deliveriesLoading && !deliveryFailure}
					<p class="text-sm text-muted-foreground">
						{filtered ? 'No deliveries match these filters.' : 'Nothing has been sent yet.'}
					</p>
				{/if}

				{#if deliveriesLoading || loadingMore}
					<p class="text-sm text-muted-foreground">Loading…</p>
				{:else if deliveryCursor}
					<Button variant="outline" class="justify-self-start" onclick={loadMore}>Load more</Button>
				{/if}
			</Card.Content>
		</Card.Root>
	{/if}
</main>

<DestructiveConfirm
	bind:open={deleteOpen}
	name={deleting?.name ?? ''}
	title="Delete this destination?"
	description={`${deleting?.name ?? 'It'} stops receiving notifications. Its delivery history goes with it, and the URL cannot be recovered.`}
	confirmLabel="Delete destination"
	onconfirm={remove}
/>

<Dialog.Root bind:open={ruleDeleteOpen}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Delete this rule?</Dialog.Title>
			<Dialog.Description>
				Alerts from this rule stop. Its destinations are kept.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (ruleDeleteOpen = false)}>Cancel</Button>
			<Button variant="destructive" onclick={removeRule}>Delete rule</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
