<script lang="ts">
	import { resolve } from '$app/paths';
	import {
		alertRuleAdmin,
		webhookAdmin,
		type AlertRule,
		type Delivery,
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
	import Send from '@lucide/svelte/icons/send';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	const EVERY = 'every';
	const kinds = Object.keys(CONDITION_LABEL) as InboxKind[];

	let destinations = $state<Webhook[]>([]);
	let deliveries = $state<Delivery[]>([]);
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
	const ruleReady = $derived(ruleTicked.length > 0 && !saving);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			[destinations, deliveries, rules, servers] = await Promise.all([
				webhookAdmin.list(),
				webhookAdmin.deliveries(),
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

	async function act(call: () => Promise<unknown>) {
		saving = true;
		failure = null;
		try {
			await call();
			await load();
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
		if (target) void act(() => webhookAdmin.remove(target.id));
	}

	function ask(w: Webhook) {
		deleting = w;
		deleteOpen = true;
	}

	function addRule() {
		void act(async () => {
			await alertRuleAdmin.create({
				condition_kind: ruleKind,
				instance_id: ruleScope === EVERY ? null : ruleScope,
				webhook_ids: ruleTicked
			});
			ruleDestinations = [];
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
		if (target) void act(() => alertRuleAdmin.remove(target.id));
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
	const event = (kind: string) => kind.replaceAll('_', ' ');
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
						<JobProgress jobId={testJob} onfinish={() => void load()} />
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
					when it clears. Quiet hours and custom thresholds can only be set through the API.
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
										{#if rule.quiet_timezone}
											<Badge variant="secondary" title="Set through the API">quiet hours</Badge>
										{/if}
										{#if Object.keys(rule.params).length > 0}
											<Badge variant="secondary" title="Set through the API"
												>custom thresholds</Badge
											>
										{/if}
										{#if !rule.enabled}<Badge variant="secondary">paused</Badge>{/if}
									</div>
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
								<Switch
									checked={rule.enabled}
									disabled={saving}
									onCheckedChange={(v) => toggleRule(rule, v)}
									aria-label="Enable the {condition(rule.condition_kind)} rule"
								/>
								<Button variant="ghost" size="sm" disabled={saving} onclick={() => askRule(rule)}>
									<Trash2 />
									<span class="sr-only">Delete rule</span>
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
						<Button class="justify-self-start" disabled={!ruleReady} onclick={addRule}>
							Add rule
						</Button>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Recent deliveries</Card.Title>
				<Card.Description>
					Every attempt is recorded, including the ones that never arrived. A send is retried three
					times over two minutes and then left here as failed.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				{#if deliveries.length === 0}
					<p class="text-sm text-muted-foreground">Nothing has been sent yet.</p>
				{:else}
					<div class="grid gap-2">
						{#each deliveries as d (d.id)}
							<div class="grid gap-1 rounded-lg border p-3">
								<div class="flex flex-wrap items-center gap-2">
									<span class="text-sm font-medium">{event(d.event_kind)}</span>
									<span class="text-sm text-muted-foreground">→ {named(d.webhook_id)}</span>
									<Badge
										variant={d.status === 'delivered'
											? 'outline'
											: d.status === 'failed'
												? 'destructive'
												: 'secondary'}
									>
										{d.status}
									</Badge>
								</div>
								<p class="text-sm text-muted-foreground">
									{when(d.created_at)} · {d.attempts} attempt{d.attempts === 1 ? '' : 's'}
								</p>
								{#if d.last_error}
									<p class="text-xs text-destructive">{d.last_error}</p>
								{/if}
							</div>
						{/each}
					</div>
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
