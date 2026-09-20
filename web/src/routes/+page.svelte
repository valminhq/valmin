<script lang="ts">
	import { resolve } from '$app/paths';
	import { actions, instances, isTransient, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import { orphans, type Orphan } from '$lib/api/instances';
	import { inbox, type InboxItem } from '$lib/api/inbox';
	import { bandItems, chipsFor } from '$lib/conditions';
	import { socketStatus } from '$lib/socket/index.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Alert from '$lib/components/ui/alert';
	import Problem from '$lib/components/problem.svelte';
	import ConditionChips from '$lib/components/condition-chips.svelte';
	import HostConditions from '$lib/components/host-conditions.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import JoinCode from '$lib/components/join-code.svelte';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import Plus from '@lucide/svelte/icons/plus';
	import Upload from '@lucide/svelte/icons/upload';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let failure = $state<unknown>(null);
	let conditionFailure = $state<unknown>(null);
	let orphanFailure = $state<unknown>(null);
	let busy = $state<string | null>(null);
	let confirming = $state<Instance | null>(null);
	let confirmOpen = $state(false);
	let orphaned = $state<Orphan[]>([]);
	let inboxItems = $state<InboxItem[]>([]);
	let checkedAt = $state<Date | null>(null);

	// Conditions are rendered where the thing they are about already is: a server's own go on
	// its card, and only those the card does not already state (ADR-195). What has no card to
	// sit on — the host's own, and any whose server is not on this list — goes to one band.
	const listedIds = $derived(instanceList.items.map((instance) => instance.id));
	const hostItems = $derived(bandItems(inboxItems, listedIds));

	// Rendered from allowed_actions, never from a role name (F3). Hiding is cosmetic: the daemon
	// checks every request regardless.
	const canCreate = $derived(session.allowedGlobally().includes(actions.create));
	const canAdopt = $derived(session.allowedGlobally().includes(actions.adopt));

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
	// could not load must not render as a clear one.
	async function loadInstances() {
		await instanceList.load();
		try {
			inboxItems = await inbox();
			checkedAt = new Date();
			conditionFailure = null;
		} catch (err) {
			conditionFailure = err;
		}
	}

	$effect(() => {
		void loadInstances();
	});

	// A reconnect re-reads the list: the socket cannot say what changed while it was gone
	// (`14 §7.2`), and the state topics were re-subscribed from scratch (ADR-041).
	let lastStatus = $state(socketStatus.value);
	$effect(() => {
		const status = socketStatus.value;
		if (status === 'open' && lastStatus !== 'open') void loadInstances();
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

	function askToDelete(instance: Instance) {
		confirming = instance;
		confirmOpen = true;
	}
</script>

<div>
	<main class="mx-auto grid max-w-4xl gap-4 p-6">
		<div class="flex flex-wrap items-center justify-between gap-4">
			<h1 class="text-2xl font-semibold tracking-tight">Servers</h1>
			{#if canCreate}
				<div class="flex flex-wrap gap-2">
					<Button variant="outline" href={resolve('/instances/import')}>
						<Upload /> Import server definition
					</Button>
					<Button href={resolve('/instances/new')}>
						<Plus />
						New server
					</Button>
				</div>
			{/if}
		</div>

		<Problem error={failure ?? instanceList.error ?? conditionFailure ?? orphanFailure} />

		{#if conditionFailure && !instanceList.error}
			<div class="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
				<span>Conditions could not be read, so this list does not say what needs attention.</span>
				<Button variant="outline" size="sm" onclick={loadInstances}>Retry</Button>
			</div>
		{/if}

		{#if orphanFailure}
			<p class="text-sm text-muted-foreground">
				Unclaimed containers could not be scanned, so this list does not say whether any exist.
			</p>
		{/if}

		<HostConditions items={hostItems} />

		<!-- Nothing flagged and nothing checked look the same otherwise, which is the reading
		     this page must not invite. -->
		{#if checkedAt && !conditionFailure}
			<p class="text-xs text-muted-foreground">
				Conditions checked {checkedAt.toLocaleTimeString()}.
			</p>
		{/if}

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

		{#if instanceList.loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if instanceList.error}
			<Button variant="outline" class="justify-self-start" onclick={loadInstances}
				>Retry loading servers</Button
			>
		{:else if instanceList.items.length === 0}
			<p class="text-sm text-muted-foreground">
				No servers yet.{#if canCreate}
					Create one to get started.
				{/if}
			</p>
		{:else}
			{#each instanceList.items as instance (instance.id)}
				{@const allowed = session.allowed(instance.id)}
				<Card.Root>
					<Card.Header>
						<!-- Identity and state own the title row at every width. Conditions sit on
						     their own line below it, where no number of them can squeeze the name. -->
						<Card.Title class="flex items-center justify-between gap-3">
							<a class="hover:underline" href={resolve('/instances/[id]', { id: instance.id })}>
								{instance.name}
							</a>
							<StateBadge state={instance.state} restartRequired={instance.restart_required} />
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
					</Card.Header>
					{#if [actions.start, actions.stop, actions.restart, actions.remove].some( (action) => allowed.includes(action) )}
						<Card.Footer class="flex flex-wrap gap-2">
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
									Start
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
									Stop
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
							{#if allowed.includes(actions.remove)}
								<Button
									variant="ghost"
									size="sm"
									class="ml-auto"
									disabled={busy === instance.id || isTransient(instance.state)}
									onclick={() => askToDelete(instance)}
								>
									<Trash2 />
									Delete
								</Button>
							{/if}
						</Card.Footer>
					{/if}
				</Card.Root>
			{/each}
		{/if}
	</main>
</div>

{#if confirming}
	{@const target = confirming}
	<DestructiveConfirm
		bind:open={confirmOpen}
		name={target.name}
		title="Delete {target.name}?"
		confirmLabel="Delete server"
		description="The container and this server's settings are removed. Its worlds are kept on disk — nothing here deletes a world."
		onconfirm={() => run(target, () => instances.remove(target.id, true))}
	/>
{/if}
