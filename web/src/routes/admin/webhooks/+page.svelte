<script lang="ts">
	import { tick } from 'svelte';
	import { resolve } from '$app/paths';
	import {
		alertRuleAdmin,
		webhookAdmin,
		type AlertRule,
		type CreateAlertRule,
		type CreateWebhook,
		type Delivery,
		type DeliveryFilter,
		type DeliveryStatus,
		type Webhook
	} from '$lib/api/admin';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { viewerZone, formatInstant } from '$lib/api/schedules';
	import { GAME_EVENTS, isGameKind } from '$lib/game-events';
	import {
		DEFAULT_STYLE,
		MAX_TEMPLATE,
		expand,
		mostCommon,
		sameStyle,
		styleOf,
		styleParams,
		type MessageStyle
	} from '$lib/message';
	import {
		DEFAULT_CRASH_COUNT,
		DEFAULT_CRASH_WINDOW_MINUTES,
		DEFAULT_STALE_FACTOR,
		DEFAULT_STUCK_MINUTES,
		HOST_WIDE,
		PANEL_EVENTS,
		PANEL_GROUPS,
		TIMED,
		isCondition,
		withoutFields,
		type PanelEvent,
		type PanelKind
	} from '$lib/notification-events';
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
	import DiscordPreview from '$lib/components/discord-preview.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import MessageStyleFields from '$lib/components/message-style-fields.svelte';
	import MessageTemplateField from '$lib/components/message-template-field.svelte';
	import Problem from '$lib/components/problem.svelte';
	import QuietHoursFields from '$lib/components/quiet-hours-fields.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import History from '@lucide/svelte/icons/history';
	import Send from '@lucide/svelte/icons/send';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import X from '@lucide/svelte/icons/x';

	interface Quiet {
		on: boolean;
		start: string;
		end: string;
		zone: string;
	}

	/** One event as the form holds it. Each `own*` flag says whether the event keeps its own value
	 * rather than the page default. Thresholds as typed: null is an empty field, the default. */
	interface Draft {
		enabled: boolean;
		template: string;
		ownChannels: boolean;
		channels: string[];
		ownQuiet: boolean;
		quiet: Quiet;
		ownStyle: boolean;
		style: MessageStyle;
		crashCount: number | null;
		crashWindowMinutes: number | null;
		stuckMinutes: number | null;
		staleFactor: number | null;
		timezone: string;
	}
	type Own = 'ownChannels' | 'ownQuiet' | 'ownStyle';

	const EVERY = 'every';
	const GROUP_NOTE: Record<PanelEvent['group'], string> = {
		'Server status': 'Sent each time it happens, and not at all during quiet hours.',
		Problems:
			'Sent when the problem starts. When it clears, the same message is edited to show it’s resolved.',
		Maintenance:
			'Sent when it starts. When it clears, the same message is edited to show it’s resolved.'
	};
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
		alert_resolved: 'Alert cleared',
		instance_auto_stopped: 'Server stopped: no players',
		power_cut_soon: 'Power cut soon',
		server_started: 'Server started',
		server_stopped: 'Server stopped',
		...Object.fromEntries(GAME_EVENTS.map((e) => [e.kind, e.headline]))
	};
	const RULE_LABEL: Record<string, string> = Object.fromEntries(
		[...PANEL_EVENTS, ...GAME_EVENTS].map((e) => [e.kind, e.label])
	);

	const localZone = viewerZone();
	const zones =
		typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [];
	const noQuiet = (): Quiet => ({ on: false, start: '22:00', end: '07:00', zone: localZone });

	// Rendered from allowed_actions, never from a role name (F3). The daemon answers 404 to a
	// caller without it, so hiding the page's controls matches what the endpoints report.
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));

	let destinations = $state<Webhook[]>([]);
	let rules = $state<AlertRule[]>([]);
	let servers = $state<Instance[]>([]);
	let loading = $state(true);
	let saving = $state(false);
	let saved = $state(false);
	let failure = $state<unknown>(null);

	let name = $state('');
	let kind = $state<CreateWebhook['kind']>('discord');
	let url = $state('');
	let testJob = $state<string | null>(null);
	let deleting = $state<Webhook | null>(null);
	let deleteOpen = $state(false);
	let deletingRule = $state<AlertRule | null>(null);
	let ruleDeleteOpen = $state(false);

	let scope = $state(EVERY);
	let postTo = $state<string[]>([]);
	let quiet = $state<Quiet>(noQuiet());
	let pageStyle = $state<MessageStyle>(DEFAULT_STYLE);
	let drafts = $state<Record<PanelKind, Draft>>(
		Object.fromEntries(PANEL_EVENTS.map((e) => [e.kind, draftFor(e)])) as Record<PanelKind, Draft>
	);
	// What was last loaded: the whole form, to tell whether anything is unsaved, and each event's
	// rule as it would be sent, to save only what changed.
	let baseline = $state('');
	let savedBodies: Partial<Record<PanelKind, string>> = {};

	let deliveries = $state.raw<Delivery[]>([]);
	let deliveryCursor = $state<string | null>(null);
	let deliveriesLoading = $state(true);
	let loadingMore = $state(false);
	let deliveryFailure = $state<unknown>(null);
	let filterDestination = $state(EVERY);
	let filterStatus = $state(EVERY);
	// The rule whose deliveries are shown, kept whole so its label outlives a reload.
	let filterRule = $state<AlertRule | null>(null);

	const ready = $derived(name.trim() !== '' && url.trim() !== '' && !saving);
	const shown = $derived(
		PANEL_EVENTS.filter((e) => scope === EVERY || !HOST_WIDE.includes(e.kind))
	);
	const scopeRules = $derived(
		rules.filter(
			(r) =>
				!isGameKind(r.condition_kind) &&
				(scope === EVERY ? r.instance_id === null : r.instance_id === scope)
		)
	);
	// A server's game event rules belong to that server's Game events tab.
	const everyServerGameRules = $derived(
		rules.filter((r) => isGameKind(r.condition_kind) && r.instance_id === null)
	);
	const snapshot = $derived(
		JSON.stringify({ drafts, postTo: [...postTo].sort(), quiet, style: styleParams(pageStyle) })
	);
	const dirty = $derived(!loading && snapshot !== baseline);
	const blocked = $derived(
		shown
			.filter((e) => drafts[e.kind].enabled)
			.map(problem)
			.find(Boolean) ?? ''
	);
	const previewServer = $derived(
		scope === EVERY ? (servers[0]?.name ?? 'Your server') : serverName(scope)
	);

	$effect(() => {
		void load();
	});

	$effect(() => {
		if (dirty) saved = false;
	});

	async function load() {
		try {
			[destinations, rules, servers] = await Promise.all([
				webhookAdmin.list(),
				alertRuleAdmin.list(),
				instances.list()
			]);
			failure = null;
			// A destination added or removed must not throw away unsaved event changes.
			if (!dirty) reset();
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	const ruleFor = (k: PanelKind) => scopeRules.find((r) => r.condition_kind === k);
	function channelsKey(ids: string[]) {
		return JSON.stringify([...ids].sort());
	}
	function quietKey(q: Quiet) {
		return q.on ? JSON.stringify([q.start, q.end, q.zone]) : '';
	}

	function quietOf(rule: AlertRule): Quiet {
		const { quiet_start_minutes: start, quiet_end_minutes: end, quiet_timezone: zone } = rule;
		if (start === null || end === null || !zone) return noQuiet();
		return { on: true, start: clock(start), end: clock(end), zone };
	}

	/** An event as its stored rule describes it, measured against the page defaults. A rule that
	 * hid fields before templates could gets the default template without their lines. */
	function draftFor(e: PanelEvent, rule?: AlertRule): Draft {
		const p = rule?.params ?? {};
		const minutes = (seconds?: number) => (seconds === undefined ? null : seconds / 60);
		const channels = rule ? [...rule.webhook_ids] : [...postTo];
		const ruleQuiet = rule ? quietOf(rule) : { ...quiet };
		const style = rule ? styleOf(p) : pageStyle;
		return {
			enabled: rule?.enabled ?? false,
			template: p.template || withoutFields(e.template, p.hidden_fields ?? []),
			ownChannels: channelsKey(channels) !== channelsKey(postTo),
			channels,
			ownQuiet: quietKey(ruleQuiet) !== quietKey(quiet),
			quiet: ruleQuiet,
			ownStyle: !sameStyle(style, pageStyle),
			style,
			crashCount: p.crash_count ?? null,
			crashWindowMinutes: minutes(p.crash_window_seconds),
			stuckMinutes: minutes(p.stuck_after_seconds),
			staleFactor: p.stale_factor ?? null,
			timezone: p.timezone ?? ''
		};
	}

	/** Puts the form back to what is stored for the scope. Each default is the value most of the
	 * scope's rules share; with one channel and nothing set up yet, that channel is ticked. */
	function reset() {
		const common = <T,>(keys: string[], fallback: T) => {
			const key = mostCommon(keys);
			return key === undefined ? fallback : (JSON.parse(key) as T);
		};
		postTo = common(
			scopeRules.map((r) => channelsKey(r.webhook_ids)),
			destinations.length === 1 ? [destinations[0].id] : []
		);
		quiet = common(
			scopeRules.map((r) => JSON.stringify(quietOf(r))),
			noQuiet()
		);
		pageStyle = common(
			scopeRules.map((r) => JSON.stringify(styleOf(r.params))),
			DEFAULT_STYLE
		);
		drafts = Object.fromEntries(
			PANEL_EVENTS.map((e) => [e.kind, draftFor(e, ruleFor(e.kind))])
		) as Record<PanelKind, Draft>;
		baseline = snapshot;
		savedBodies = Object.fromEntries(shown.map((e) => [e.kind, bodyKey(e)]));
	}

	/** An event's rule as it would be sent, with each default resolved. */
	function bodyFor(e: PanelEvent): CreateAlertRule {
		const d = drafts[e.kind];
		const q = d.ownQuiet ? d.quiet : quiet;
		const template = d.template.trim();
		const seconds = (minutes: number | null) =>
			minutes === null ? undefined : Math.round(minutes * 60);
		const params: AlertRule['params'] = {};
		if (e.kind === 'crash_loop') {
			if (d.crashCount !== null) params.crash_count = d.crashCount;
			if (d.crashWindowMinutes !== null)
				params.crash_window_seconds = seconds(d.crashWindowMinutes);
		} else if (e.kind === 'job_stuck') {
			if (d.stuckMinutes !== null) params.stuck_after_seconds = seconds(d.stuckMinutes);
		} else if (e.kind === 'stale_backup') {
			if (d.staleFactor !== null) params.stale_factor = d.staleFactor;
		}
		if (TIMED.includes(e.kind) && d.timezone.trim()) params.timezone = d.timezone.trim();
		// The default is stored as no template, so a better default reaches this rule too.
		if (template && template !== e.template) params.template = template;
		return {
			condition_kind: e.kind,
			instance_id: HOST_WIDE.includes(e.kind) || scope === EVERY ? null : scope,
			webhook_ids: d.ownChannels ? d.channels : postTo,
			params: { ...params, ...styleParams(d.ownStyle ? d.style : pageStyle) },
			quiet_start_minutes: q.on ? minutesOf(q.start) : 0,
			quiet_end_minutes: q.on ? minutesOf(q.end) : 0,
			quiet_timezone: q.on ? q.zone.trim() : ''
		};
	}

	const bodyKey = (e: PanelEvent) =>
		JSON.stringify({ ...bodyFor(e), enabled: drafts[e.kind].enabled });

	// Mirrors the daemon: counts and durations are 0 or more, a stale factor 0 or above 1.
	const badCount = (d: Draft) =>
		d.crashCount !== null && !(Number.isInteger(d.crashCount) && d.crashCount >= 0);
	const badMinutes = (m: number | null) => m !== null && !(m >= 0);
	const badFactor = (d: Draft) =>
		d.staleFactor !== null && d.staleFactor !== 0 && !(d.staleFactor > 1);
	// The daemon reads a window whose start equals its end as empty.
	const badQuiet = (q: Quiet) =>
		q.on && (q.start === '' || q.end === '' || q.zone.trim() === '' || q.start === q.end);

	/** Why a switched-on event cannot be saved, or ''. */
	function problem(e: PanelEvent): string {
		const d = drafts[e.kind];
		if ((d.ownChannels ? d.channels : postTo).length === 0)
			return `Choose at least one channel for ${e.label}.`;
		if (d.template.length > MAX_TEMPLATE)
			return `The ${e.label} message can be at most ${MAX_TEMPLATE} characters.`;
		if (badQuiet(d.ownQuiet ? d.quiet : quiet))
			return `Quiet hours for ${e.label} need a start, a different end and a timezone.`;
		if (
			(e.kind === 'crash_loop' && (badCount(d) || badMinutes(d.crashWindowMinutes))) ||
			(e.kind === 'job_stuck' && badMinutes(d.stuckMinutes)) ||
			(e.kind === 'stale_backup' && badFactor(d))
		)
			return `Check the numbers for ${e.label}.`;
		return '';
	}

	async function save() {
		saving = true;
		saved = false;
		failure = null;
		try {
			for (const e of shown) {
				const draft = drafts[e.kind];
				const rule = ruleFor(e.kind);
				const body = bodyFor(e);
				if (rule && bodyKey(e) !== savedBodies[e.kind]) {
					// The daemon ignores a null instance_id on a patch; an empty one clears the server.
					await alertRuleAdmin.update(rule.id, {
						...body,
						instance_id: body.instance_id ?? '',
						enabled: draft.enabled
					});
				} else if (!rule && draft.enabled) {
					await alertRuleAdmin.create(body);
				}
			}
			rules = await alertRuleAdmin.list();
			reset();
			saved = true;
		} catch (err) {
			failure = err;
			// Rules saved before the failure are kept; reading them back keeps a retry from adding
			// them twice.
			rules = await alertRuleAdmin.list().catch(() => rules);
		} finally {
			saving = false;
		}
	}

	function changeScope(value: string) {
		scope = value;
		reset();
	}

	/** Lets an event keep its own value, starting from the default, or go back to the default. */
	function own(k: PanelKind, field: Own, on: boolean) {
		const d = drafts[k];
		d[field] = on;
		if (field === 'ownChannels') d.channels = [...postTo];
		if (field === 'ownQuiet') d.quiet = { ...quiet };
		if (field === 'ownStyle') d.style = pageStyle;
	}

	const onForEvery = (k: PanelKind) =>
		scope !== EVERY &&
		rules.some((r) => r.condition_kind === k && r.instance_id === null && r.enabled);
	const preview = (e: PanelEvent) =>
		expand(drafts[e.kind].template.trim() || e.template, {
			...e.sample,
			server: HOST_WIDE.includes(e.kind) ? '' : previewServer,
			event: e.headline
		});

	const deliveryFilter = $derived<DeliveryFilter>({
		webhook_id: filterDestination === EVERY ? undefined : filterDestination,
		status: STATUSES.find((s) => s === filterStatus),
		rule_id: filterRule?.id
	});
	const filtered = $derived(Object.values(deliveryFilter).some(Boolean));

	// Re-runs whenever a filter changes, which also discards the old list's cursor.
	$effect(() => {
		void reloadDeliveries(deliveryFilter);
	});

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
			if (filterRule?.id === target.id) filterRule = null;
		});
	}

	const toggled = (list: string[], item: string, on: boolean) =>
		on ? [...list, item] : list.filter((other) => other !== item);
	function serverName(id: string | null) {
		return id === null
			? 'Every server'
			: (servers.find((s) => s.id === id)?.name ?? 'a deleted server');
	}
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
	const event = (k: string) => EVENT_LABEL[k] ?? k.replaceAll('_', ' ');
	const when = (iso: string) => formatInstant(iso);
</script>

{#snippet channels(selected: string[], onchange: (ids: string[]) => void)}
	<div class="flex flex-wrap gap-2">
		{#each destinations as d (d.id)}
			<label
				class="flex cursor-pointer items-center gap-2 rounded-md border px-3 py-1.5 text-sm has-[:checked]:border-primary has-[:checked]:bg-primary/5 has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring"
			>
				<input
					type="checkbox"
					class="accent-primary"
					checked={selected.includes(d.id)}
					onchange={(ev) => onchange(toggled(selected, d.id, ev.currentTarget.checked))}
				/>
				{d.name}
			</label>
		{/each}
	</div>
{/snippet}

{#snippet sameAs(k: PanelKind, field: Own, label: string)}
	<label class="flex items-center gap-2 text-sm">
		<input
			type="checkbox"
			class="accent-primary"
			checked={!drafts[k][field]}
			onchange={(ev) => own(k, field, !ev.currentTarget.checked)}
		/>
		{label}
	</label>
{/snippet}

<main class="mx-auto grid max-w-3xl gap-6 p-6 pb-24">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Notifications</h1>
		<p class="text-sm text-muted-foreground">
			Choose what the panel posts to your channels and how each message looks. A server that stops
			on its own, a new game build and a failed backup are posted to every channel unless an event
			below already covers them.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">Notification settings are not available to you.</p>
	{:else}
		<Card.Root>
			<Card.Header>
				<Card.Title>Channels</Card.Title>
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
						No channels yet. Add one below, then send a test notification.
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

		{#if !loading}
			<section class="grid gap-4 rounded-lg border p-4" data-testid="defaults">
				<div class="flex flex-wrap items-end justify-between gap-3">
					<div class="grid gap-0.5">
						<h2 class="text-lg font-semibold tracking-tight">Events</h2>
						<p class="text-sm text-muted-foreground">
							Every event uses these settings unless it sets its own.
						</p>
					</div>
					<div class="grid gap-1.5">
						<Label for="scope">Settings for</Label>
						<Select.Root type="single" value={scope} onValueChange={changeScope} disabled={dirty}>
							<Select.Trigger id="scope" class="min-w-48"
								>{serverName(scope === EVERY ? null : scope)}</Select.Trigger
							>
							<Select.Content>
								<Select.Item value={EVERY}>Every server</Select.Item>
								{#each servers as s (s.id)}
									<Select.Item value={s.id}>{s.name}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					</div>
				</div>
				{#if dirty}
					<p class="text-xs text-muted-foreground">
						Save or discard your changes to switch servers.
					</p>
				{/if}
				{#if scope !== EVERY}
					<p class="text-sm text-muted-foreground">
						Events switched on for every server are sent for {serverName(scope)} too. Its players, bosses
						and raids are on its
						<a class="underline" href={resolve('/instances/[id]/game-events', { id: scope })}
							>Game events</a
						> tab.
					</p>
				{/if}

				<div class="grid gap-2">
					<h3 class="text-sm font-medium">Post to</h3>
					{#if destinations.length === 0}
						<p class="text-sm text-muted-foreground">Add a channel above first.</p>
					{:else}
						{@render channels(postTo, (ids) => (postTo = ids))}
					{/if}
				</div>
				<div class="grid gap-3">
					<QuietHoursFields
						id="default-quiet"
						zonesList="rule-zones"
						bind:on={quiet.on}
						bind:start={quiet.start}
						bind:end={quiet.end}
						bind:zone={quiet.zone}
						invalid={badQuiet(quiet)}
						hint="Alerts still open when quiet hours end are sent then; one that opens and clears inside the window is not sent."
					/>
				</div>
				<div class="grid gap-2">
					<h3 class="text-sm font-medium">Message style</h3>
					<MessageStyleFields id="default-style" bind:style={pageStyle} />
				</div>
			</section>

			{#each PANEL_GROUPS as group (group)}
				{@const events = shown.filter((e) => e.group === group)}
				<section class="grid gap-3" aria-labelledby="group-{group}">
					<div class="grid gap-0.5">
						<h3 id="group-{group}" class="text-lg font-semibold tracking-tight">{group}</h3>
						<p class="text-sm text-muted-foreground">{GROUP_NOTE[group]}</p>
					</div>
					<ul class="divide-y rounded-lg border">
						{#each events as e (e.kind)}
							{@const d = drafts[e.kind]}
							{@const rule = ruleFor(e.kind)}
							<li class="grid gap-3 p-4" data-testid="event-{e.kind}">
								<div class="flex items-start gap-3">
									<Switch
										id="toggle-{e.kind}"
										class="mt-0.5"
										bind:checked={drafts[e.kind].enabled}
										aria-describedby="when-{e.kind}"
									/>
									<div class="grid flex-1 gap-0.5">
										<div class="flex flex-wrap items-center gap-2">
											<label for="toggle-{e.kind}" class="text-sm font-medium">{e.label}</label>
											{#if onForEvery(e.kind)}
												<Badge variant="secondary">On for every server</Badge>
											{/if}
										</div>
										<p id="when-{e.kind}" class="text-sm text-muted-foreground">{e.when}</p>
									</div>
									{#if rule}
										<Button variant="ghost" size="sm" onclick={() => showDeliveries(rule)}>
											<History /> Deliveries
										</Button>
									{/if}
								</div>

								{#if d.enabled}
									<div class="grid gap-4 sm:pl-12">
										{#if e.kind === 'crash_loop'}
											<div class="grid gap-3 sm:grid-cols-2">
												<div class="grid gap-2">
													<Label for="crash-count">Stops</Label>
													<Input
														id="crash-count"
														type="number"
														min="0"
														step="1"
														placeholder={String(DEFAULT_CRASH_COUNT)}
														aria-invalid={badCount(d)}
														bind:value={drafts[e.kind].crashCount}
													/>
												</div>
												<div class="grid gap-2">
													<Label for="crash-window">Within minutes</Label>
													<Input
														id="crash-window"
														type="number"
														min="0"
														placeholder={String(DEFAULT_CRASH_WINDOW_MINUTES)}
														aria-invalid={badMinutes(d.crashWindowMinutes)}
														bind:value={drafts[e.kind].crashWindowMinutes}
													/>
												</div>
											</div>
										{:else if e.kind === 'job_stuck'}
											<div class="grid gap-2 sm:w-1/2">
												<Label for="stuck-minutes">Minutes a job may run</Label>
												<Input
													id="stuck-minutes"
													type="number"
													min="0"
													placeholder={String(DEFAULT_STUCK_MINUTES)}
													aria-invalid={badMinutes(d.stuckMinutes)}
													bind:value={drafts[e.kind].stuckMinutes}
												/>
											</div>
										{:else if e.kind === 'stale_backup'}
											<div class="grid gap-2 sm:w-1/2">
												<Label for="stale-factor">Times the backup interval</Label>
												<Input
													id="stale-factor"
													type="number"
													step="0.1"
													placeholder={String(DEFAULT_STALE_FACTOR)}
													aria-invalid={badFactor(d)}
													bind:value={drafts[e.kind].staleFactor}
												/>
											</div>
										{/if}

										<MessageTemplateField
											id="message-{e.kind}"
											bind:value={drafts[e.kind].template}
											fallback={e.template}
											placeholders={e.placeholders}
										/>

										{#if TIMED.includes(e.kind)}
											<div class="grid gap-2 sm:w-1/2">
												<Label for="zone-{e.kind}">Timezone for times in the message</Label>
												<Input
													id="zone-{e.kind}"
													list="rule-zones"
													placeholder="UTC"
													bind:value={drafts[e.kind].timezone}
												/>
											</div>
										{/if}

										<div class="grid gap-3">
											{@render sameAs(e.kind, 'ownChannels', 'Post to the default channels')}
											{#if d.ownChannels}
												{@render channels(d.channels, (ids) => (drafts[e.kind].channels = ids))}
											{/if}
											{@render sameAs(e.kind, 'ownQuiet', 'Use the default quiet hours')}
											{#if d.ownQuiet}
												<QuietHoursFields
													id="quiet-{e.kind}"
													zonesList="rule-zones"
													bind:on={drafts[e.kind].quiet.on}
													bind:start={drafts[e.kind].quiet.start}
													bind:end={drafts[e.kind].quiet.end}
													bind:zone={drafts[e.kind].quiet.zone}
													invalid={badQuiet(d.quiet)}
													hint="Applies to this event only."
												/>
											{/if}
											{@render sameAs(e.kind, 'ownStyle', 'Use the default style')}
											{#if d.ownStyle}
												<MessageStyleFields
													id="style-{e.kind}"
													bind:style={drafts[e.kind].style}
													accent={e.accent}
												/>
											{/if}
										</div>

										<DiscordPreview
											data-testid="preview-{e.kind}"
											server={HOST_WIDE.includes(e.kind) ? '' : previewServer}
											headline={e.headline}
											text={preview(e)}
											style={d.ownStyle ? d.style : pageStyle}
											accent={e.accent}
										/>
										{#if isCondition(e.kind)}
											<p class="text-xs text-muted-foreground">
												When it clears, this message is edited to show it’s resolved.
											</p>
										{/if}
									</div>
								{/if}
							</li>
						{/each}
					</ul>
				</section>
			{/each}

			<datalist id="rule-zones">
				{#each zones as zone (zone)}<option value={zone}></option>{/each}
			</datalist>

			{#if scope === EVERY && everyServerGameRules.length > 0}
				<section class="grid gap-2">
					<h3 class="text-sm font-medium">Game events for every server</h3>
					<p class="text-sm text-muted-foreground">
						Game events are now set up on each server’s Game events tab.
					</p>
					{#each everyServerGameRules as r (r.id)}
						<div
							class="flex flex-wrap items-center gap-3 rounded-lg border p-3"
							data-testid="game-rule-{r.id}"
						>
							<span class="flex-1 text-sm">
								{RULE_LABEL[r.condition_kind]} → {r.webhook_ids.map(named).join(', ') || 'nowhere'}
							</span>
							<Button variant="ghost" size="sm" disabled={saving} onclick={() => askRule(r)}>
								<Trash2 />
								<span class="sr-only">Delete the {RULE_LABEL[r.condition_kind]} rule</span>
							</Button>
						</div>
					{/each}
				</section>
			{/if}

			{#if saved && !dirty}
				<p class="text-sm text-muted-foreground" role="status">Changes saved.</p>
			{/if}
		{/if}

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
							Only messages sent for {RULE_LABEL[filterRule.condition_kind] ??
								filterRule.condition_kind}
							on {filterRule.instance_id ? serverName(filterRule.instance_id) : 'every server'}.
							Messages sent before the panel began recording their rule are not listed.
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

{#if allowed && dirty}
	<div
		class="fixed inset-x-0 bottom-0 z-20 border-t bg-background/95 px-4 py-3 backdrop-blur"
		role="region"
		aria-label="Unsaved changes"
	>
		<div class="mx-auto flex max-w-3xl flex-wrap items-center justify-end gap-3">
			<p class="mr-auto text-sm {blocked ? 'text-destructive' : 'text-muted-foreground'}">
				{blocked || 'You have unsaved changes.'}
			</p>
			<Button variant="outline" disabled={saving} onclick={reset}>Discard</Button>
			<Button disabled={saving || blocked !== ''} onclick={save}>
				{saving ? 'Saving…' : 'Save changes'}
			</Button>
		</div>
	</div>
{/if}

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
				Messages from this rule stop. Its destinations are kept.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (ruleDeleteOpen = false)}>Cancel</Button>
			<Button variant="destructive" onclick={removeRule}>Delete rule</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
