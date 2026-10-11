<script lang="ts">
	import { formatInstant } from '$lib/api/schedules';
	import { resolve } from '$app/paths';
	import {
		actions,
		instances,
		isTransient,
		stateSentence,
		type Instance
	} from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import { orphans, type Orphan } from '$lib/api/instances';
	import { inbox, type InboxItem } from '$lib/api/inbox';
	import { bandItems, chipsFor } from '$lib/conditions';
	import { filterServers, type ServerStateFilter } from '$lib/server-filter';
	import { socketStatus } from '$lib/socket/index.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import * as Card from '$lib/components/ui/card';
	import * as Alert from '$lib/components/ui/alert';
	import Problem from '$lib/components/problem.svelte';
	import ConditionChips from '$lib/components/condition-chips.svelte';
	import HostConditions from '$lib/components/host-conditions.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import JoinCode from '$lib/components/join-code.svelte';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import Plus from '@lucide/svelte/icons/plus';
	import Upload from '@lucide/svelte/icons/upload';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let failure = $state<unknown>(null);
	let conditionFailure = $state<unknown>(null);
	let orphanFailure = $state<unknown>(null);
	let busy = $state<string | null>(null);
	let orphaned = $state<Orphan[]>([]);
	let inboxItems = $state<InboxItem[]>([]);
	let checkedAt = $state<Date | null>(null);
	let refreshing = $state(false);
	let query = $state('');
	let stateFilter = $state<ServerStateFilter>('all');

	/** How often the conditions are re-read while the page is visible. */
	const CONDITIONS_PERIOD_MS = 60_000;
	/** The list gains search and a state filter only above this many servers. */
	const FILTER_ABOVE = 6;

	const filterable = $derived(instanceList.items.length > FILTER_ABOVE);
	const shown = $derived(
		filterable
			? filterServers(instanceList.items, inboxItems, query, stateFilter)
			: instanceList.items
	);

	// Conditions are rendered where the thing they are about already is: a server's own go on
	// its card, and only those the card does not already state (ADR-195). What has no card to
	// sit on — the host's own, and any whose server is not on this list — goes to one band.
	const listedIds = $derived(instanceList.items.map((instance) => instance.id));
	const hostItems = $derived(bandItems(inboxItems, listedIds));

	// Rendered from allowed_actions, never from a role name (F3). Hiding is cosmetic: the daemon
	// checks every request regardless.
	const canCreate = $derived(session.allowedGlobally().includes(actions.create));
	const canAdopt = $derived(session.allowedGlobally().includes(actions.adopt));
	const empty = $derived(
		!instanceList.loading && !instanceList.error && instanceList.items.length === 0
	);

	/** What a card says its server needs, for the states that cannot be acted on from here. */
	function needs(instance: Instance): string | null {
		if (instance.state === 'error')
			return `${stateSentence('error')} Open it to see what went wrong.`;
		if (instance.state === 'created') return `${stateSentence('created')} Open it to finish setup.`;
		return null;
	}

	// An orphan has no instance row and so no detail page (`08 §6.1`), which is why it is
	// reported on the list. The dedicated action is admin-only (`09 §3.3`). A scan that could
	// not run is kept as a failure: no orphans and no answer are different facts.
	$effect(() => {
		if (!canAdopt) {
			orphaned = [];
			return;
		}
		void orphans()
			.then((found) => ((orphaned = found), (orphanFailure = null)))
			.catch((err) => (orphanFailure = err));
	});

	// A conditions read that failed is reported rather than emptied: an attention summary that
	// could not load must not render as a clear one. A read already in flight is joined, so
	// two answers never land out of order.
	let conditionsRead: Promise<void> | null = null;
	function readConditions(): Promise<void> {
		conditionsRead ??= inbox()
			.then((items) => {
				inboxItems = items;
				checkedAt = new Date();
				conditionFailure = null;
			})
			.catch((err) => {
				conditionFailure = err;
			})
			.finally(() => {
				conditionsRead = null;
			});
		return conditionsRead;
	}

	let refreshesInFlight = 0;
	/** Re-reads the server list, then the conditions; busy until every overlapping refresh ends. */
	async function refresh() {
		refreshesInFlight++;
		refreshing = true;
		try {
			await instanceList.load();
			await readConditions();
		} finally {
			refreshing = --refreshesInFlight > 0;
		}
	}

	$effect(() => {
		void refresh();
	});

	// Conditions have no live feed, so they are re-read on a period while the page is seen,
	// and at once on coming back to it if the last answer has aged past that period.
	$effect(() => {
		const timer = setInterval(() => {
			if (document.visibilityState === 'visible') void readConditions();
		}, CONDITIONS_PERIOD_MS);
		const onVisibility = () => {
			if (document.visibilityState !== 'visible') return;
			if (!checkedAt || Date.now() - checkedAt.getTime() >= CONDITIONS_PERIOD_MS) {
				void readConditions();
			}
		};
		document.addEventListener('visibilitychange', onVisibility);
		return () => {
			clearInterval(timer);
			document.removeEventListener('visibilitychange', onVisibility);
		};
	});

	function clearFilters() {
		query = '';
		stateFilter = 'all';
	}

	// A reconnect re-reads the list: the socket cannot say what changed while it was gone
	// (`14 §7.2`), and the state topics were re-subscribed from scratch (ADR-041).
	let lastStatus = $state(socketStatus.value);
	$effect(() => {
		const status = socketStatus.value;
		if (status === 'open' && lastStatus !== 'open') void refresh();
		lastStatus = status;
	});

	async function run(instance: Instance, action: () => Promise<unknown>) {
		busy = instance.id;
		failure = null;
		try {
			await action();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}
</script>

<div>
	<main class="mx-auto grid max-w-4xl gap-4 p-6">
		<div class="flex flex-wrap items-center justify-between gap-4">
			<h1 class="text-2xl font-semibold tracking-tight">Servers</h1>
			{#if canCreate && !empty}
				<div class="flex flex-wrap gap-2">
					<Button variant="outline" href={resolve('/instances/import')}>
						<Upload /> New from template
					</Button>
					<Button href={resolve('/instances/new')}>
						<Plus />
						New server
					</Button>
				</div>
			{/if}
		</div>

		<Problem error={failure ?? instanceList.error ?? conditionFailure ?? orphanFailure} />

		{#if orphanFailure}
			<p class="text-sm text-muted-foreground">
				Unclaimed containers could not be scanned, so this list does not say whether any exist.
			</p>
		{/if}

		<!-- Nothing flagged and nothing checked look the same otherwise, which is the reading
		     this page must not invite. -->
		{#if !instanceList.error}
			<div class="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
				{#if conditionFailure}
					<span>Conditions could not be read, so this list does not say what needs attention.</span>
				{:else if checkedAt}
					<span>Conditions checked {formatInstant(checkedAt, { timeStyle: 'medium' })}.</span>
				{/if}
				<Button variant="outline" size="sm" disabled={refreshing} onclick={refresh}>
					<RefreshCw />
					{conditionFailure ? 'Retry' : 'Refresh'}
				</Button>
			</div>
		{/if}

		<HostConditions items={hostItems} />

		{#if orphaned.length > 0}
			<Alert.Root>
				<TriangleAlert />
				<Alert.Title>
					{orphaned.length}
					{orphaned.length === 1 ? 'container is' : 'containers are'} not claimed by any server
				</Alert.Title>
				<Alert.Description>
					<div class="grid gap-2">
						<p>
							Valmin created these containers, but their server settings are missing from the panel.
							Review a container to recover management of the server.
						</p>
						{#each orphaned as orphan (orphan.container_id)}
							<div class="flex flex-wrap items-center justify-between gap-2 rounded-md border p-2">
								<span>{orphan.name} · udp {orphan.base_port}–{orphan.base_port + 1}</span>
								<Button
									variant="outline"
									size="sm"
									href={resolve('/instances/adopt/[container_id]', {
										container_id: orphan.container_id
									})}
								>
									Review and recover
								</Button>
							</div>
						{/each}
					</div>
				</Alert.Description>
			</Alert.Root>
		{/if}

		<!-- A refresh keeps the cards on screen; only a first load has nothing to show. -->
		{#if instanceList.loading && instanceList.items.length === 0}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if instanceList.error}
			<Button variant="outline" class="justify-self-start" onclick={refresh}
				>Retry loading servers</Button
			>
		{:else if instanceList.items.length === 0}
			<div class="grid justify-items-center gap-2 rounded-lg border border-dashed p-8 text-center">
				<p class="font-medium">No servers yet</p>
				<p class="text-sm text-muted-foreground">
					{canCreate
						? 'Create a server from scratch, or start from a template someone shared.'
						: 'Servers you are given access to show here.'}
				</p>
				{#if canCreate}
					<div class="mt-2 flex flex-wrap justify-center gap-2">
						<Button href={resolve('/instances/new')}><Plus /> New server</Button>
						<Button variant="outline" href={resolve('/instances/import')}>
							<Upload /> New from template
						</Button>
					</div>
				{/if}
			</div>
		{:else}
			{#if filterable}
				<div class="grid gap-3 sm:grid-cols-[1fr_auto] sm:items-end">
					<div class="grid gap-2">
						<Label for="server-search">Search by name</Label>
						<Input id="server-search" type="search" bind:value={query} />
					</div>
					<div class="grid gap-2">
						<Label for="server-state">State</Label>
						<select
							id="server-state"
							class="h-8 rounded-lg border border-input bg-background px-2.5 text-sm focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none"
							bind:value={stateFilter}
						>
							<option value="all">All</option>
							<option value="running">Running</option>
							<option value="stopped">Stopped</option>
							<option value="attention">Needs attention</option>
						</select>
					</div>
				</div>
				<p class="text-sm text-muted-foreground" role="status">
					{shown.length} of {instanceList.items.length} servers
				</p>
			{/if}
			{#each shown as instance (instance.id)}
				{@const allowed = session.allowed(instance.id)}
				{@const need = needs(instance)}
				<Card.Root>
					<Card.Header>
						<!-- Identity and state own the title row at every width. Conditions sit on
						     their own line below it, where no number of them can squeeze the name. -->
						<Card.Title class="flex items-center justify-between gap-3">
							<a class="hover:underline" href={resolve('/instances/[id]', { id: instance.id })}>
								{instance.name}
							</a>
							<StateBadge
								state={instance.state}
								restartRequired={instance.restart_required}
								pendingRestart={instance.pending_restart}
							/>
						</Card.Title>
						<ConditionChips items={chipsFor(inboxItems, instance.id)} />
						<Card.Description class="flex flex-wrap items-center gap-x-2 gap-y-1">
							{#if instance.crossplay_join_code}
								<JoinCode code={instance.crossplay_join_code} />
							{/if}
							<span>
								{instance.server_name} · world {instance.world_name} · udp {instance.base_port}–{instance.base_port +
									1}
							</span>
						</Card.Description>
						{#if need}
							<p class="text-sm text-muted-foreground">{need}</p>
						{/if}
					</Card.Header>
					<Card.Footer class="flex flex-wrap gap-2">
						<!-- One emphasised action per state: the thing to do with a stopped server is
						     start it, and with any other it is to go to it. -->
						<Button
							variant={instance.state === 'stopped' && allowed.includes(actions.start)
								? 'outline'
								: 'default'}
							size="sm"
							href={resolve('/instances/[id]', { id: instance.id })}
						>
							Open server
						</Button>
						{#if allowed.includes(actions.start)}
							<Button
								variant={instance.state === 'stopped' ? 'default' : 'outline'}
								size="sm"
								disabled={busy === instance.id ||
									isTransient(instance.state) ||
									instance.state !== 'stopped'}
								onclick={() => run(instance, () => instances.start(instance.id))}
							>
								<Play />
								{instance.state === 'starting' ? 'Starting…' : 'Start'}
							</Button>
						{/if}
						{#if allowed.includes(actions.stop)}
							<Button
								variant="outline"
								size="sm"
								disabled={busy === instance.id ||
									isTransient(instance.state) ||
									instance.state !== 'running'}
								onclick={() => run(instance, () => instances.stop(instance.id))}
							>
								<Square />
								{instance.state === 'stopping' ? 'Stopping…' : 'Stop'}
							</Button>
						{/if}
						{#if allowed.includes(actions.restart)}
							<Button
								variant="outline"
								size="sm"
								disabled={busy === instance.id ||
									isTransient(instance.state) ||
									instance.state !== 'running'}
								onclick={() => run(instance, () => instances.restart(instance.id))}
							>
								<RotateCw />
								Restart
							</Button>
						{/if}
					</Card.Footer>
				</Card.Root>
			{:else}
				<div
					class="grid justify-items-center gap-2 rounded-lg border border-dashed p-6 text-center"
				>
					<p class="font-medium">No servers match</p>
					<p class="text-sm text-muted-foreground">No server matches this search and state.</p>
					<Button variant="outline" size="sm" onclick={clearFilters}>Clear filters</Button>
				</div>
			{/each}
		{/if}
	</main>
</div>
