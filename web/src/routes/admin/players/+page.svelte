<script lang="ts">
	import PageHeader from '#lib/components/page-header.svelte';
	import { pageNarrow } from '#lib/layout.js';
	import { addToPlayerList, hostPlayers, type HostPlayer } from '#lib/api/players.js';
	import { actions, instances, type Instance } from '#lib/api/instances.js';
	import { formatInstant } from '#lib/api/schedules.js';
	import { session } from '#lib/state/session.svelte.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import Ban from '@lucide/svelte/icons/ban';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import CopyButton from '#lib/components/copy-button.svelte';
	import PlayerNames from '#lib/components/player-names.svelte';
	import Problem from '#lib/components/problem.svelte';

	const allowed = $derived(session.allowedGlobally().includes(actions.playersManage));
	let players = $state<HostPlayer[]>([]);
	let query = $state('');
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let servers = $state<Instance[]>([]);

	// The account being banned, the servers ticked for it, and each server's outcome once run.
	let target = $state<HostPlayer | null>(null);
	let open = $state(false);
	let chosen = $state<string[]>([]);
	let results = $state<Record<string, { text: string; failed: boolean }>>({});
	let banning = $state(false);

	const shown = $derived.by(() => {
		const q = query.trim().toLowerCase();
		if (q === '') return players;
		return players.filter(
			(p) =>
				p.platform_id.toLowerCase().includes(q) ||
				p.names.some((n) => n.name.toLowerCase().includes(q)) ||
				p.servers.some(
					(s) => s.instance_name.toLowerCase().includes(q) || s.name.toLowerCase().includes(q)
				)
		);
	});

	async function load() {
		loading = true;
		try {
			[players, servers] = await Promise.all([hostPlayers.list(), instances.list()]);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}
	$effect(() => {
		if (allowed) void load();
	});

	/** Opens the ban dialog with every server ticked, or only the ones the account was seen on. */
	function askBan(player: HostPlayer, everywhere: boolean) {
		target = player;
		const seenOn = new Set(player.servers.map((s) => s.instance_id));
		chosen = servers.filter((s) => everywhere || seenOn.has(s.id)).map((s) => s.id);
		results = {};
		open = true;
	}

	/** Adds the account to each ticked server's ban list in turn; one failure does not stop the rest. */
	async function ban() {
		if (!target) return;
		const account = target.platform_id;
		banning = true;
		for (const id of [...chosen]) {
			try {
				const outcome = await addToPlayerList(id, 'bans', account);
				results[id] = { text: outcome === 'added' ? 'Banned' : 'Already banned', failed: false };
			} catch (err) {
				const text = err instanceof Error ? err.message : 'Valmin could not complete the action.';
				results[id] = { text, failed: true };
			}
		}
		banning = false;
	}
</script>

<main class={pageNarrow}>
	<PageHeader title="Players">
		Every account any server has seen, with the names it was seen with. The ID is the account; a
		name in grey was matched by timing and is believed once it keeps coming back with the same ID.
	</PageHeader>
	{#if !allowed}
		<p class="text-sm text-muted-foreground">
			You do not have permission to see players across servers.
		</p>
	{:else}
		<Problem error={failure} />

		<div class="grid gap-2">
			<Label for="player-search">Search by name or ID</Label>
			<Input id="player-search" type="search" bind:value={query} />
		</div>

		<Card.Root>
			<Card.Content class="grid gap-3">
				{#if loading}
					<p class="text-sm text-muted-foreground">Loading players…</p>
				{:else if failure}
					<p class="text-sm text-muted-foreground">The list could not be loaded.</p>
				{:else if shown.length === 0}
					<p class="text-sm text-muted-foreground">
						{players.length === 0 ? 'No server has seen a player yet.' : 'No player matches.'}
					</p>
				{:else}
					<ul class="grid gap-2">
						{#each shown as player (player.platform_id)}
							<li class="grid gap-1 rounded-md border p-3">
								<div class="flex flex-wrap items-center justify-between gap-2">
									<span class="flex flex-wrap items-baseline gap-2">
										<PlayerNames names={player.names} />
										<span class="font-mono text-xs break-all">{player.platform_id}</span>
									</span>
									<span class="flex flex-wrap gap-1">
										<Button
											variant="ghost"
											size="sm"
											disabled={banning}
											onclick={() => askBan(player, true)}
										>
											<Ban />
											<span>Ban on every server</span>
										</Button>
										<Button
											variant="ghost"
											size="sm"
											disabled={banning}
											onclick={() => askBan(player, false)}
										>
											<Ban />
											<span>Ban on chosen servers</span>
										</Button>
										<CopyButton
											value={player.platform_id}
											label="Copy ID"
											variant="ghost"
											size="sm"
											ariaLabel="Copy {player.platform_id}"
										/>
									</span>
								</div>
								<span class="text-xs text-muted-foreground">
									last seen {formatInstant(player.last_seen_at)} on
									{player.servers.map((s) => s.instance_name).join(', ')}
								</span>
							</li>
						{/each}
					</ul>
				{/if}
			</Card.Content>
		</Card.Root>
	{/if}
</main>

<Dialog.Root bind:open>
	<!-- Held open while banning, so every server's outcome stays in view. -->
	<Dialog.Content
		showCloseButton={!banning}
		onEscapeKeydown={(e) => banning && e.preventDefault()}
		onInteractOutside={(e) => banning && e.preventDefault()}
	>
		{#if target}
			<Dialog.Header>
				<Dialog.Title>Ban {target.platform_id}</Dialog.Title>
				<Dialog.Description>
					Adds the account to the Banned list of each server ticked below. Each server's list is
					saved on its own.
				</Dialog.Description>
			</Dialog.Header>
			<fieldset class="grid gap-2" disabled={banning}>
				<legend class="sr-only">Servers</legend>
				{#each servers as server (server.id)}
					<label class="flex flex-wrap items-center gap-2 text-sm">
						<input
							type="checkbox"
							class="size-4 accent-primary"
							value={server.id}
							bind:group={chosen}
						/>
						{server.name}
						{#if results[server.id]}
							<span
								class={[
									'text-xs',
									results[server.id].failed ? 'text-destructive' : 'text-muted-foreground'
								]}>{results[server.id].text}</span
							>
						{/if}
					</label>
				{/each}
			</fieldset>
			<Dialog.Footer>
				<Button variant="outline" disabled={banning} onclick={() => (open = false)}>Close</Button>
				<Button disabled={banning || chosen.length === 0} onclick={() => void ban()}>
					{banning ? 'Banning…' : `Ban on ${chosen.length} server${chosen.length === 1 ? '' : 's'}`}
				</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
