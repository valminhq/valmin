<script lang="ts">
	import { socket } from '#lib/socket/index.svelte.js';
	import PageHeader from '#lib/components/page-header.svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import {
		actions,
		instances,
		type CommandCapabilities,
		type Instance
	} from '#lib/api/instances.js';
	import { seenPlayers, type PlayerListKind, type SeenPlayer } from '#lib/api/players.js';
	import { session } from '#lib/state/session.svelte.js';
	import { instanceList } from '#lib/state/instances.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import JobProgress from '#lib/components/job-progress.svelte';
	import PlayerHistory from '#lib/components/player-history.svelte';
	import PlayerListEditor from '#lib/components/player-list-editor.svelte';
	import SeenPlayers from '#lib/components/seen-players.svelte';
	import Problem from '#lib/components/problem.svelte';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);
	let restartOpen = $state(false);
	let restartJob = $state<string | null>(null);
	let seen = $state<SeenPlayer[]>([]);
	let seenLoaded = $state(false);
	let seenFailure = $state<unknown>(null);
	let capabilities = $state<CommandCapabilities | null>(null);
	let revisions = $state<Record<PlayerListKind, number>>({ admins: 0, bans: 0, permitted: 0 });

	const allowed = $derived(session.allowed(id));
	const canManage = $derived(allowed.includes(actions.playersManage));
	const canRestart = $derived(allowed.includes(actions.restart));
	const canStats = $derived(allowed.includes(actions.statsRead));
	// The layout keeps this list live, so Kick follows the server starting and stopping.
	const running = $derived(
		(instanceList.items.find((row) => row.id === id) ?? instance)?.state === 'running'
	);
	const canKick = $derived(
		running &&
			capabilities?.command_channel === 'rcon' &&
			allowed.includes(actions.commandsSend) &&
			(capabilities.allowed_commands.length === 0 || capabilities.allowed_commands.includes('kick'))
	);

	// Waits for the permissions; only the server id re-runs it.
	$effect(() => {
		if (!session.ready) return;
		void id;
		untrack(() => void load());
	});
	// A first read that failed while the panel was unreachable is tried again when it comes back.
	$effect(() => socket.onConnected(() => void (!instance && load())));

	async function load() {
		try {
			if (session.allowed(id).length === 0) await session.refreshPermissions();
			instance = await instances.get(id);
			failure = null;
			const may = session.allowed(id);
			if (may.includes(actions.playersManage)) void loadSeen();
			if (may.includes(actions.commandsSend)) {
				capabilities = await instances.capabilities(id).catch(() => null);
			}
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function loadSeen() {
		try {
			seen = (await seenPlayers.list(id)).items;
			seenFailure = null;
		} catch (err) {
			seenFailure = err;
		} finally {
			seenLoaded = true;
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

<div class="grid gap-6">
	<PageHeader title="Players" level={2}>
		Player activity, and who may join and run admin commands.
	</PageHeader>

	<Problem error={failure} />

	<!-- Outside the list editors' gate: the history is authorized on stats.read, which a
	     viewer holds, and the lists are not. -->
	{#if instance && canStats}
		<PlayerHistory instanceId={id} />
	{/if}

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<!-- A failed read is explained by the problem above. -->
		{#if !failure}
			<p class="text-sm text-muted-foreground">This server is not here.</p>
		{/if}
	{:else if !canManage}
		<p class="text-sm text-muted-foreground">
			You cannot read or change this server’s player lists.
		</p>
	{:else}
		<SeenPlayers
			instanceId={id}
			players={seen}
			loaded={seenLoaded}
			failure={seenFailure}
			{canKick}
			crossplay={instance.crossplay}
			onadded={(kind) => revisions[kind]++}
		/>

		<section class="grid gap-4" aria-labelledby="player-lists">
			<div class="flex flex-wrap items-end justify-between gap-3">
				<div class="grid gap-1">
					<h3 id="player-lists" class="text-lg font-semibold">Player lists</h3>
					<p id="player-lists-hint" class="text-sm text-muted-foreground">
						One ID per line, bare or platform-prefixed, kept as entered. Comments already in a list
						are kept but not shown.
					</p>
					<p class="text-sm text-muted-foreground">
						Saving a list does not restart the server. The game usually picks up the change while
						running; if it does not, restart the server.
					</p>
				</div>
				{#if canRestart}
					<Button
						variant="outline"
						size="sm"
						disabled={instance.state !== 'running' || restartJob !== null}
						onclick={() => (restartOpen = true)}
					>
						<RotateCw />
						Restart server
					</Button>
				{/if}
			</div>

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

			<div class="grid gap-4 lg:grid-cols-3">
				<PlayerListEditor
					instanceId={id}
					kind="admins"
					players={seen}
					revision={revisions.admins}
					title="Admins"
					description="Players allowed to use in-game administrator commands."
					hintId="player-lists-hint"
				/>
				<PlayerListEditor
					instanceId={id}
					kind="bans"
					players={seen}
					revision={revisions.bans}
					title="Banned"
					description="Players refused when they try to join this server."
					hintId="player-lists-hint"
				/>
				<PlayerListEditor
					instanceId={id}
					kind="permitted"
					players={seen}
					revision={revisions.permitted}
					title="Allowlist"
					description="When it has anyone on it, only these players may join."
					hintId="player-lists-hint"
				/>
			</div>
		</section>
	{/if}
</div>

<Dialog.Root bind:open={restartOpen}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Restart this server?</Dialog.Title>
			<Dialog.Description>
				Connected players are disconnected while the server saves the world and restarts. Restart
				only if a saved list has not taken effect.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (restartOpen = false)}>Cancel</Button>
			<Button onclick={() => void restart()}>Restart</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
