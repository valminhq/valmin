<script lang="ts">
	import { tick } from 'svelte';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { notifications, type AlertRule, type Webhook } from '$lib/api/notifications';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import ChannelList from '$lib/components/channel-list.svelte';
	import DeliveryList from '$lib/components/delivery-list.svelte';
	import Problem from '$lib/components/problem.svelte';
	import RuleEditor from '$lib/components/rule-editor.svelte';
	import RuleList from '$lib/components/rule-list.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import Plus from '@lucide/svelte/icons/plus';

	const api = notifications();

	// Rendered from allowed_actions, never from a role name (F3). The daemon answers 404 to a
	// caller without it, so hiding the page's controls matches what the endpoints report.
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));

	let channels = $state<Webhook[]>([]);
	let rules = $state<AlertRule[]>([]);
	let servers = $state<Instance[]>([]);
	let loading = $state(true);
	let busy = $state(false);
	let failure = $state<unknown>(null);
	let filterRule = $state<AlertRule | null>(null);
	let editor = $state<RuleEditor>();
	let history = $state<DeliveryList>();

	const shared = $derived(channels.filter((c) => c.instance_id === null));
	const panelRules = $derived(rules.filter((r) => r.instance_id === null));
	const perServer = $derived(
		servers.map((s) => ({
			server: s,
			rules: rules.filter((r) => r.instance_id === s.id).length,
			channels: channels.filter((c) => c.instance_id === s.id).length
		}))
	);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			[channels, rules, servers] = await Promise.all([
				api.webhooks.list(),
				api.rules.list(),
				instances.list()
			]);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function refresh() {
		void load();
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
			document.getElementById('deliveries')?.scrollIntoView({ block: 'start', behavior: 'smooth' })
		);
	}

	const serverName = (id: string) => servers.find((s) => s.id === id)?.name ?? 'a deleted server';
	const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;
</script>

<main class="mx-auto grid max-w-3xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Notifications</h1>
		<p class="text-sm text-muted-foreground">
			Shared channels and the rules that cover every server. Each server sets up its own channels
			and rules on its Notifications tab.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">Notification settings are not available to you.</p>
	{:else if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else}
		<Card.Root>
			<Card.Header>
				<Card.Title>Shared channels</Card.Title>
				<Card.Description>
					Every server’s rules can post to these. A Discord webhook URL is a password: anyone
					holding it can post to that channel. The panel stores it encrypted and never shows it
					again, so keep your own copy if you need one.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<ChannelList {api} channels={shared} onchange={refresh} />
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Rules for every server</Card.Title>
				<Card.Description>
					What the panel posts about any server, including servers added later, and about the host.
				</Card.Description>
				<Card.Action>
					<Button size="sm" disabled={busy} onclick={() => editor?.add()}><Plus /> Add rule</Button>
				</Card.Action>
			</Card.Header>
			<Card.Content>
				{#if panelRules.length === 0}
					<p class="text-sm text-muted-foreground">
						{shared.length === 0
							? 'Add a shared channel first, then a rule to post to it.'
							: 'No rules yet. Add one to choose what gets posted.'}
					</p>
				{:else}
					<RuleList
						rules={panelRules}
						{channels}
						{busy}
						ontoggle={(r, enabled) => act(() => api.rules.update(r.id, { enabled }))}
						onedit={(r) => editor?.edit(r)}
						onduplicate={(r) => editor?.duplicate(r)}
						ondelete={removeRule}
						ondeliveries={showDeliveries}
					/>
				{/if}
			</Card.Content>
		</Card.Root>

		{#if perServer.length > 0}
			<Card.Root>
				<Card.Header>
					<Card.Title>Server rules</Card.Title>
					<Card.Description>Each server’s own channels and rules.</Card.Description>
				</Card.Header>
				<Card.Content>
					<ul class="divide-y rounded-lg border">
						{#each perServer as { server, rules: ruleCount, channels: channelCount } (server.id)}
							<li>
								<a
									class="flex items-center gap-3 p-3 hover:bg-muted/50"
									href={resolve('/instances/[id]/notifications', { id: server.id })}
								>
									<span class="flex-1 text-sm font-medium">{server.name}</span>
									<span class="text-sm text-muted-foreground">
										{plural(ruleCount, 'rule')} · {plural(channelCount, 'own channel')}
									</span>
									<ChevronRight class="size-4 text-muted-foreground" />
								</a>
							</li>
						{/each}
					</ul>
				</Card.Content>
			</Card.Root>
		{/if}

		<Card.Root id="deliveries">
			<Card.Header>
				<Card.Title>Recent deliveries</Card.Title>
				<Card.Description>
					Every attempt is recorded, including the ones that never arrived. A send is retried three
					times over two minutes and then left here as failed.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<DeliveryList bind:this={history} {api} {channels} {serverName} bind:rule={filterRule} />
			</Card.Content>
		</Card.Root>

		<RuleEditor bind:this={editor} {api} channels={shared} server="" hostWide onsaved={refresh} />
	{/if}
</main>
