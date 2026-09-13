<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Alert from '$lib/components/ui/alert';
	import * as Dialog from '$lib/components/ui/dialog';
	import JobProgress from '$lib/components/job-progress.svelte';
	import PlayerHistory from '$lib/components/player-history.svelte';
	import PlayerListEditor from '$lib/components/player-list-editor.svelte';
	import SeenPlayers from '$lib/components/seen-players.svelte';
	import Problem from '$lib/components/problem.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);
	let restartOpen = $state(false);
	let restartJob = $state<string | null>(null);

	const allowed = $derived(session.allowed(id));
	const canManage = $derived(allowed.includes(actions.playersManage));
	const canRestart = $derived(allowed.includes(actions.restart));
	const canStats = $derived(allowed.includes(actions.statsRead));

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			if (session.allowed(id).length === 0) await session.refreshPermissions();
			instance = await instances.get(id);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function restart() {
		restartOpen = false;
		failure = null;
		try {
			restartJob = (await instances.restart(id)).job_id;
		} catch (err) {
			failure = err;
		}
	}
</script>

<div class="mx-auto grid max-w-4xl gap-6 p-6">
	<header class="grid gap-3">
		<Button
			variant="ghost"
			size="sm"
			class="justify-self-start"
			href={resolve('/instances/[id]', { id })}
		>
			<ArrowLeft />
			{instance?.name ?? 'Server'}
		</Button>
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h1 class="text-lg font-semibold">Player access</h1>
				<p class="text-sm text-muted-foreground">
					Admin, ban, and permitted lists used by the game server.
				</p>
			</div>
			{#if instance}
				<StateBadge state={instance.state} restartRequired={instance.restart_required} />
			{/if}
		</div>
	</header>

	<Problem error={failure} />

	<!-- Outside the list editors' gate: the history is authorized on stats.read, which a
	     viewer holds, and the lists are not. -->
	{#if instance && canStats}
		<PlayerHistory instanceId={id} />
	{/if}

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else if !canManage}
		<p class="text-sm text-muted-foreground">
			You cannot read or change this server’s player lists.
		</p>
	{:else}
		<Alert.Root>
			<TriangleAlert />
			<Alert.Title>A save may not take effect immediately</Alert.Title>
			<Alert.Description class="grid gap-2">
				<p>
					The game usually reloads these files while running, but that behavior is not guaranteed.
					Saving a list does not restart the server.
				</p>
				{#if canRestart}
					<Button
						variant="outline"
						size="sm"
						class="justify-self-start"
						disabled={instance.state !== 'running' || restartJob !== null}
						onclick={() => (restartOpen = true)}
					>
						<RotateCw />
						Restart server
					</Button>
				{/if}
			</Alert.Description>
		</Alert.Root>

		{#if restartJob}
			<div class="rounded-lg border p-4">
				<JobProgress
					jobId={restartJob}
					onfinish={() => {
						restartJob = null;
						void load();
					}}
				/>
			</div>
		{/if}

		<SeenPlayers instanceId={id} />

		<div class="grid gap-4 lg:grid-cols-3">
			<PlayerListEditor
				instanceId={id}
				kind="admins"
				title="Admins"
				description="Players allowed to use in-game administrator commands."
			/>
			<PlayerListEditor
				instanceId={id}
				kind="bans"
				title="Banned"
				description="Players refused when they try to join this server."
			/>
			<PlayerListEditor
				instanceId={id}
				kind="permitted"
				title="Permitted"
				description="When non-empty, only these players may join."
			/>
		</div>
	{/if}
</div>

<Dialog.Root bind:open={restartOpen}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Restart this server?</Dialog.Title>
			<Dialog.Description>
				Connected players will be disconnected while the ordinary restart job saves and restarts the
				world. Restart only if a saved list has not taken effect.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (restartOpen = false)}>Cancel</Button>
			<Button onclick={() => void restart()}>Restart</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
