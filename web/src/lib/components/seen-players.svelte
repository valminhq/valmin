<script lang="ts">
	import { seenPlayers, type SeenPlayer } from '$lib/api/players';
	import * as Card from '$lib/components/ui/card';
	import Problem from '$lib/components/problem.svelte';
	import CopyButton from '$lib/components/copy-button.svelte';

	let { instanceId }: { instanceId: string } = $props();

	let players = $state<SeenPlayer[]>([]);
	let failure = $state<unknown>(null);
	let loaded = $state(false);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			players = (await seenPlayers.list(instanceId)).items;
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loaded = true;
		}
	}

	function when(iso: string): string {
		return new Date(iso).toLocaleString();
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Seen on this server</Card.Title>
		<Card.Description>
			Accounts this server named in its log. Copy an ID into a list below. Seen means the account
			reached this server, not that it played or was let in.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-3">
		<Problem error={failure} />
		{#if !loaded}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if players.length === 0}
			<p class="text-sm text-muted-foreground">
				Nobody yet. An account appears here the first time it reaches this server while the panel is
				reading the log.
			</p>
		{:else}
			<ul class="grid gap-2">
				{#each players as player (player.platform_id)}
					<li class="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
						<span class="grid gap-0.5">
							<span class="flex flex-wrap items-baseline gap-2">
								{#if player.name}
									<span class="text-sm">{player.name}</span>
								{/if}
								<span class="font-mono text-xs break-all">{player.platform_id}</span>
							</span>
							<span class="text-xs text-muted-foreground"
								>last seen {when(player.last_seen_at)}</span
							>
						</span>
						<CopyButton
							value={player.platform_id}
							label="Copy ID"
							variant="ghost"
							size="sm"
							ariaLabel="Copy {player.platform_id}"
						/>
					</li>
				{/each}
			</ul>
		{/if}
	</Card.Content>
</Card.Root>
