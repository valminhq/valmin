<script lang="ts">
	import { tick } from 'svelte';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '#lib/api/instances.js';
	import { notifications, type AlertRule, type Webhook } from '#lib/api/notifications.js';
	import { isGameKind } from '#lib/game-events.js';
	import { HOST_WIDE } from '#lib/notification-events.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import ChannelList from '#lib/components/channel-list.svelte';
	import DeliveryList from '#lib/components/delivery-list.svelte';
	import Problem from '#lib/components/problem.svelte';
	import RuleEditor from '#lib/components/rule-editor.svelte';
	import RuleList from '#lib/components/rule-list.svelte';
	import Plus from '@lucide/svelte/icons/plus';

	const PLUGIN_CONFIG = 'io.valmin.eye.cfg';

	const id = $derived(page.params.id ?? '');
	const api = $derived(notifications(id));
	// Rendered from allowed_actions, never from a role name (F3).
	const allowed = $derived(session.allowed(id).includes(actions.notificationsManage));
	const admin = $derived(session.allowedGlobally().includes(actions.panelSettings));

	let instance = $state<Instance | null>(null);
	// The installed ValminEye version: null without it, undefined while unknown.
	let plugin = $state<string | null | undefined>(undefined);
	let channels = $state<Webhook[]>([]);
	let rules = $state<AlertRule[]>([]);
	let loading = $state(true);
	let busy = $state(false);
	let failure = $state<unknown>(null);
	let filterRule = $state<AlertRule | null>(null);
	let editor = $state<RuleEditor>();
	let history = $state<DeliveryList>();

	const own = $derived(rules.filter((r) => r.instance_id === id));
	// Host-wide rules post about the host, never about this server.
	const panelWide = $derived(
		rules.filter((r) => r.instance_id === null && !HOST_WIDE.includes(r.condition_kind))
	);
	const gameRules = $derived(rules.some((r) => r.enabled && isGameKind(r.condition_kind)));

	$effect(() => {
		if (allowed) void load(id);
	});

	async function load(instanceId: string) {
		const scoped = notifications(instanceId);
		try {
			[instance, channels, rules] = await Promise.all([
				instances.get(instanceId),
				scoped.webhooks.list(),
				scoped.rules.list()
			]);
			failure = null;
			void instances.capabilities(instanceId).then(
				(c) => (plugin = c.valmin_eye),
				() => (plugin = undefined)
			);
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function refresh() {
		void load(id);
		history?.reload();
	}

	async function act(call: () => Promise<unknown>) {
		busy = true;
		failure = null;
		try {
			await call();
			refresh();
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	const removeRule = (rule: AlertRule) =>
		act(async () => {
			await api.rules.remove(rule.id);
			if (filterRule?.id === rule.id) filterRule = null;
		});

	/** Narrows the delivery list to what one rule sent and brings the list into view. */
	function showDeliveries(rule: AlertRule) {
		filterRule = rule;
		void tick().then(() =>
			document
				.getElementById('server-deliveries')
				?.scrollIntoView({ block: 'start', behavior: 'smooth' })
		);
	}
</script>

<div class="grid gap-8">
	<header class="grid gap-1">
		<h2 class="text-2xl font-semibold tracking-tight">Notifications</h2>
		<p class="max-w-prose text-sm text-muted-foreground">
			Post what happens on this server to your channels: when it starts and stops, problems,
			updates, and what players do in the game.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">
			Managing this server’s notifications is not available to you.
		</p>
	{:else if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else}
		{#if gameRules || plugin}
			<section
				class="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-lg border p-4"
				data-testid="plugin-status"
			>
				{#if plugin === undefined}
					<p class="text-sm text-muted-foreground">Checking for the ValminEye plugin…</p>
				{:else if plugin === null}
					<div class="grid flex-1 gap-1">
						<p class="text-sm font-medium">Game events need the ValminEye plugin</p>
						<p class="text-sm text-muted-foreground">
							The plugin notices what happens inside the game. Install it, then restart the server.
							Players don’t need to install anything.
						</p>
					</div>
					<Button href={resolve(`/instances/[id]/mods?q=ValminEye`, { id })}
						>Install ValminEye</Button
					>
				{:else}
					<p class="flex-1 text-sm">ValminEye {plugin} is installed.</p>
					<Button
						variant="outline"
						size="sm"
						href={resolve('/instances/[id]/configs/[file]', { id, file: PLUGIN_CONFIG })}
					>
						Plugin settings
					</Button>
				{/if}
			</section>
		{/if}

		<section class="grid gap-3" aria-labelledby="channels-heading">
			<div class="grid gap-0.5">
				<h3 id="channels-heading" class="text-lg font-semibold tracking-tight">Channels</h3>
				<p class="text-sm text-muted-foreground">
					This server’s own channels, and the shared ones every server can post to. A Discord
					webhook URL is a password: the panel stores it encrypted and never shows it again.
				</p>
			</div>

			<ChannelList {api} {channels} server={id} onchange={refresh} />
		</section>

		<section class="grid gap-3" aria-labelledby="rules-heading">
			<div class="flex flex-wrap items-end justify-between gap-3">
				<div class="grid gap-0.5">
					<h3 id="rules-heading" class="text-lg font-semibold tracking-tight">Rules</h3>
					<p class="text-sm text-muted-foreground">
						What the panel posts about this server, and where.
					</p>
				</div>
				<Button size="sm" disabled={busy} onclick={() => editor?.add()}><Plus /> Add rule</Button>
			</div>
			{#if own.length === 0}
				<p class="text-sm text-muted-foreground">
					{channels.length === 0
						? 'Add a channel first, then a rule to post to it.'
						: 'No rules yet. Add one to choose what gets posted.'}
				</p>
			{:else}
				<RuleList
					rules={own}
					{channels}
					{busy}
					ontoggle={(r, enabled) => act(() => api.rules.update(r.id, { enabled }))}
					onedit={(r) => editor?.edit(r)}
					onduplicate={(r) => editor?.duplicate(r)}
					ondelete={removeRule}
					ondeliveries={showDeliveries}
				/>
			{/if}

			{#if panelWide.length > 0}
				<div class="grid gap-2 pt-2" data-testid="panel-rules">
					<h4 class="text-sm font-medium">
						Rules for every server, which also post about this one
					</h4>
					<p class="text-sm text-muted-foreground">
						{#if admin}
							Change them in
							<a class="underline" href={resolve('admin/webhooks')}
								>Administration › Notifications</a
							>.
						{:else}
							A panel administrator manages these.
						{/if}
					</p>

					<RuleList rules={panelWide} {channels} ondeliveries={showDeliveries} />
				</div>
			{/if}
		</section>

		<section id="server-deliveries" class="grid gap-3" aria-labelledby="deliveries-heading">
			<div class="grid gap-0.5">
				<h3 id="deliveries-heading" class="text-lg font-semibold tracking-tight">
					Recent deliveries
				</h3>
				<p class="text-sm text-muted-foreground">
					Every message about this server, including the ones that never arrived. A send is retried
					three times over two minutes and then left here as failed.
				</p>
			</div>

			<DeliveryList bind:this={history} {api} {channels} bind:rule={filterRule} />
		</section>

		<RuleEditor
			bind:this={editor}
			{api}
			{channels}
			server={instance?.name ?? ''}
			pluginMissing={plugin === null}
			onsaved={refresh}
		/>
	{/if}
</div>
