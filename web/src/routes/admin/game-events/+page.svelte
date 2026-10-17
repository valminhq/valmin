<script lang="ts">
	import { resolve } from '$app/paths';
	import {
		alertRuleAdmin,
		webhookAdmin,
		type AlertRule,
		type CreateAlertRule,
		type GameKind,
		type Webhook
	} from '$lib/api/admin';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { viewerZone } from '$lib/api/schedules';
	import { GAME_EVENTS, expand, isGameKind } from '$lib/game-events';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Select from '$lib/components/ui/select';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import QuietHoursFields from '$lib/components/quiet-hours-fields.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Pencil from '@lucide/svelte/icons/pencil';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	const EVERY = 'every';
	const PLUGIN_CONFIG = 'io.valmin.eye.cfg';
	const textareaClass =
		'w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-destructive/20';
	const zones =
		typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [];
	const localZone = viewerZone();

	let destinations = $state<Webhook[]>([]);
	let rules = $state<AlertRule[]>([]);
	let servers = $state<Instance[]>([]);
	// Each server's installed ValminEye version: null without it, undefined while unknown.
	let plugin = $state<Record<string, string | null | undefined>>({});
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	let editing = $state<AlertRule | null>(null);
	let ruleKind = $state<GameKind>('player_joined');
	let ruleServer = $state(EVERY);
	let ruleDestinations = $state<string[]>([]);
	let template = $state('');
	let quietOn = $state(false);
	let quietStart = $state('22:00');
	let quietEnd = $state('07:00');
	let quietZone = $state(localZone);
	let deleting = $state<AlertRule | null>(null);
	let deleteOpen = $state(false);

	// Rendered from allowed_actions, never from a role name (F3).
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));
	const gameRules = $derived(rules.filter((r) => isGameKind(r.condition_kind)));
	const selected = $derived(GAME_EVENTS.find((e) => e.kind === ruleKind) ?? GAME_EVENTS[0]);
	const preview = $derived(expand(template.trim() || selected.template, selected.sample));
	const badQuiet = $derived(
		quietOn &&
			(quietStart === '' || quietEnd === '' || quietZone.trim() === '' || quietStart === quietEnd)
	);
	const ready = $derived(
		!saving && ruleDestinations.length > 0 && template.length <= 1000 && !badQuiet
	);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			[destinations, rules, servers] = await Promise.all([
				webhookAdmin.list(),
				alertRuleAdmin.list(),
				instances.list()
			]);
			failure = null;
			void loadPlugin(servers);
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	/** Reads which servers have ValminEye. A server whose capabilities fail to load stays unknown. */
	async function loadPlugin(list: Instance[]) {
		const found = await Promise.all(
			list.map((s) =>
				instances.capabilities(s.id).then(
					(c) => [s.id, c.valmin_eye] as const,
					() => [s.id, undefined] as const
				)
			)
		);
		plugin = Object.fromEntries(found);
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

	/** Loads a rule into the form for editing, or clears the form for a new rule. */
	function fill(rule: AlertRule | null) {
		editing = rule;
		ruleKind = rule && isGameKind(rule.condition_kind) ? rule.condition_kind : 'player_joined';
		ruleServer = rule?.instance_id ?? EVERY;
		ruleDestinations = rule ? [...rule.webhook_ids] : [];
		template = rule?.params.template ?? '';
		quietOn = !!rule?.quiet_timezone;
		quietStart = clock(rule?.quiet_start_minutes ?? 22 * 60);
		quietEnd = clock(rule?.quiet_end_minutes ?? 7 * 60);
		quietZone = rule?.quiet_timezone ?? localZone;
	}

	function save() {
		const target = editing;
		const body: CreateAlertRule = {
			condition_kind: ruleKind,
			instance_id: ruleServer === EVERY ? null : ruleServer,
			webhook_ids: ruleDestinations,
			params: template.trim() ? { template: template.trim() } : {},
			quiet_start_minutes: quietOn ? minutesOf(quietStart) : 0,
			quiet_end_minutes: quietOn ? minutesOf(quietEnd) : 0,
			quiet_timezone: quietOn ? quietZone.trim() : ''
		};
		void act(async () => {
			// The daemon ignores a null instance_id on a patch; an empty one clears the server.
			if (target) {
				await alertRuleAdmin.update(target.id, { ...body, instance_id: body.instance_id ?? '' });
			} else {
				await alertRuleAdmin.create(body);
			}
			fill(null);
		});
	}

	function remove() {
		const target = deleting;
		deleteOpen = false;
		if (!target) return;
		void act(async () => {
			await alertRuleAdmin.remove(target.id);
			if (editing?.id === target.id) fill(null);
		});
	}

	const toggled = (list: string[], item: string, on: boolean) =>
		on ? [...list, item] : list.filter((other) => other !== item);
	const label = (kind: string) => GAME_EVENTS.find((e) => e.kind === kind)?.label ?? kind;
	const serverName = (id: string | null) =>
		id === null ? 'Every server' : (servers.find((s) => s.id === id)?.name ?? 'a deleted server');
	const named = (id: string) =>
		destinations.find((w) => w.id === id)?.name ?? 'a deleted destination';
	function clock(minutes: number) {
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${pad(Math.floor(minutes / 60))}:${pad(minutes % 60)}`;
	}
	function minutesOf(hhmm: string) {
		return Number(hhmm.slice(0, 2)) * 60 + Number(hhmm.slice(3, 5));
	}
</script>

<main class="mx-auto grid max-w-3xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Game events</h1>
		<p class="text-sm text-muted-foreground">
			With the ValminEye plugin on a server, the panel can post who joined and left, boss fights,
			first boss kills and raids to your <a class="underline" href={resolve('/admin/webhooks')}
				>notification destinations</a
			>. Players need nothing on their side.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">Notification settings are not available to you.</p>
	{:else}
		<Card.Root>
			<Card.Header>
				<Card.Title>Servers</Card.Title>
				<Card.Description>
					Only servers with ValminEye report game events. Restart a server after installing it.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-2">
				{#if loading}
					<p class="text-sm text-muted-foreground">Loading…</p>
				{:else if servers.length === 0}
					<p class="text-sm text-muted-foreground">No servers yet.</p>
				{/if}
				{#each servers as server (server.id)}
					{@const version = plugin[server.id]}
					<div
						class="flex flex-wrap items-center gap-3 rounded-lg border p-3"
						data-testid="eye-{server.id}"
					>
						<span class="flex-1 text-sm font-medium">{server.name}</span>
						{#if version === undefined}
							<span class="text-sm text-muted-foreground">Checking…</span>
						{:else if version === null}
							<Badge variant="secondary">Not installed</Badge>
							<Button
								variant="outline"
								size="sm"
								href={resolve(`/instances/[id]/mods?q=ValminEye`, { id: server.id })}
							>
								Install
							</Button>
						{:else}
							<Badge variant="outline">ValminEye {version}</Badge>
							<Button
								variant="outline"
								size="sm"
								href={resolve('/instances/[id]/configs/[file]', {
									id: server.id,
									file: PLUGIN_CONFIG
								})}
							>
								Configure
							</Button>
						{/if}
					</div>
				{/each}
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Rules</Card.Title>
				<Card.Description>
					Each rule sends one game event to the destinations it names. The message is a template: a
					name in braces, such as <code>{'{player}'}</code>, is replaced by that field of the event.
					A field turned off in the plugin's config is left empty.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				{#if !loading && gameRules.length === 0}
					<p class="text-sm text-muted-foreground">No game event rules yet.</p>
				{/if}
				{#each gameRules as rule (rule.id)}
					<div
						class="flex flex-wrap items-center gap-3 rounded-lg border p-3"
						data-testid="game-rule-{rule.id}"
					>
						<div class="grid flex-1 gap-1">
							<div class="flex flex-wrap items-center gap-2">
								<span class="text-sm font-medium">{label(rule.condition_kind)}</span>
								<Badge variant="outline">{serverName(rule.instance_id)}</Badge>
								{#if !rule.enabled}<Badge variant="secondary">paused</Badge>{/if}
							</div>
							<p class="text-sm text-muted-foreground">
								{rule.params.template ?? 'Default message'} → {rule.webhook_ids
									.map(named)
									.join(', ') || 'no destinations'}
							</p>
						</div>
						<Switch
							checked={rule.enabled}
							disabled={saving}
							onCheckedChange={(enabled) =>
								void act(() => alertRuleAdmin.update(rule.id, { enabled }))}
							aria-label="Enable the {label(rule.condition_kind)} rule"
						/>
						<Button variant="ghost" size="sm" disabled={saving} onclick={() => fill(rule)}>
							<Pencil />
							<span class="sr-only">Edit the {label(rule.condition_kind)} rule</span>
						</Button>
						<Button
							variant="ghost"
							size="sm"
							disabled={saving}
							onclick={() => {
								deleting = rule;
								deleteOpen = true;
							}}
						>
							<Trash2 />
							<span class="sr-only">Delete the {label(rule.condition_kind)} rule</span>
						</Button>
					</div>
				{/each}

				<div class="grid gap-3 border-t pt-4">
					{#if !loading && destinations.length === 0}
						<p class="text-sm text-muted-foreground">
							Add a destination on the <a class="underline" href={resolve('/admin/webhooks')}
								>Notifications</a
							> page before adding a rule.
						</p>
					{:else}
						<div class="grid gap-3 sm:grid-cols-2">
							<div class="grid gap-2">
								<Label for="game-event">Event</Label>
								<Select.Root
									type="single"
									value={ruleKind}
									onValueChange={(v) => (ruleKind = v as GameKind)}
								>
									<Select.Trigger id="game-event">{label(ruleKind)}</Select.Trigger>
									<Select.Content>
										{#each GAME_EVENTS as e (e.kind)}
											<Select.Item value={e.kind}>{e.label}</Select.Item>
										{/each}
									</Select.Content>
								</Select.Root>
							</div>
							<div class="grid gap-2">
								<Label for="game-server">Server</Label>
								<Select.Root type="single" bind:value={ruleServer}>
									<Select.Trigger id="game-server">
										{ruleServer === EVERY ? 'Every server' : serverName(ruleServer)}
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
							{#each destinations as d (d.id)}
								<label class="flex items-center gap-2 text-sm">
									<input
										type="checkbox"
										checked={ruleDestinations.includes(d.id)}
										onchange={(e) =>
											(ruleDestinations = toggled(ruleDestinations, d.id, e.currentTarget.checked))}
									/>
									{d.name}
								</label>
							{/each}
						</fieldset>

						<div class="grid gap-2">
							<Label for="game-template">Message</Label>
							<textarea
								id="game-template"
								rows="3"
								class={textareaClass}
								placeholder={selected.template}
								aria-invalid={template.length > 1000}
								bind:value={template}></textarea>
							<div class="flex flex-wrap items-center gap-1 text-xs text-muted-foreground">
								Fields:
								{#each selected.placeholders as name (name)}
									<Button
										variant="outline"
										size="sm"
										class="h-6 px-2 font-mono text-xs"
										onclick={() => (template = `${template || selected.template} {${name}}`)}
									>
										{`{${name}}`}
									</Button>
								{/each}
							</div>
							<p class="text-sm" data-testid="game-preview">
								<span class="text-muted-foreground">Preview:</span>
								{preview}
							</p>
							{#if template.length > 1000}
								<p class="text-xs text-destructive">At most 1000 characters.</p>
							{/if}
						</div>

						<QuietHoursFields
							id="game-quiet"
							zonesList="game-zones"
							bind:on={quietOn}
							bind:start={quietStart}
							bind:end={quietEnd}
							bind:zone={quietZone}
							invalid={badQuiet}
							hint="Events inside quiet hours are not sent."
						/>
						<datalist id="game-zones">
							{#each zones as zone (zone)}<option value={zone}></option>{/each}
						</datalist>

						<div class="flex flex-wrap gap-2">
							<Button disabled={!ready} onclick={save}>{editing ? 'Save rule' : 'Add rule'}</Button>
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
	{/if}
</main>

<Dialog.Root bind:open={deleteOpen}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Delete this rule?</Dialog.Title>
			<Dialog.Description
				>Messages from this rule stop. Its destinations are kept.</Dialog.Description
			>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (deleteOpen = false)}>Cancel</Button>
			<Button variant="destructive" onclick={remove}>Delete rule</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
