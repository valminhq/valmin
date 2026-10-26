<script lang="ts">
	import { hostPlayers, type HostPlayer } from '#lib/api/players.js';
	import { actions } from '#lib/api/instances.js';
	import { formatInstant } from '#lib/api/schedules.js';
	import { session } from '#lib/state/session.svelte.js';
	import * as Card from '#lib/components/ui/card/index.js';
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

	const shown = $derived.by(() => {
		const q = query.trim().toLowerCase();
		if (q === '') return players;
		return players.filter(
			(p) =>
				p.platform_id.toLowerCase().includes(q) ||
				p.names.some((n) => n.name.toLowerCase().includes(q)) ||
				p.servers.some((s) => s.name.toLowerCase().includes(q))
		);
	});

	async function load() {
		loading = true;
		try {
			players = await hostPlayers.list();
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
</script>

<main class="mx-auto grid w-full max-w-3xl gap-6 p-4 sm:p-6">
	<h1 class="text-2xl font-semibold">Players</h1>
	{#if !allowed}
		<p>You do not have permission to see players across servers.</p>
	{:else}
		<p class="text-sm text-muted-foreground">
			Every account any server has seen, with the names it was seen with. The ID is the account; a
			name in grey was matched by timing and is believed once it keeps coming back with the same ID.
		</p>
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
									<CopyButton
										value={player.platform_id}
										label="Copy ID"
										variant="ghost"
										size="sm"
										ariaLabel="Copy {player.platform_id}"
									/>
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
