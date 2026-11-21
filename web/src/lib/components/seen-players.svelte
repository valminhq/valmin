<script lang="ts">
	import { formatInstant } from '#lib/api/schedules.js';
	import { instances } from '#lib/api/instances.js';
	import { addToPlayerList, type PlayerListKind, type SeenPlayer } from '#lib/api/players.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import Problem from '#lib/components/problem.svelte';
	import CopyButton from '#lib/components/copy-button.svelte';
	import PlayerNames from '#lib/components/player-names.svelte';
	import Ban from '@lucide/svelte/icons/ban';
	import ShieldPlus from '@lucide/svelte/icons/shield-plus';
	import UserCheck from '@lucide/svelte/icons/user-check';
	import UserX from '@lucide/svelte/icons/user-x';

	let {
		instanceId,
		players,
		loaded,
		failure = null,
		canKick = false,
		crossplay = false,
		onadded
	}: {
		instanceId: string;
		players: SeenPlayer[];
		loaded: boolean;
		failure?: unknown;
		/** Whether the server is running and the caller may send `kick` to it. */
		canKick?: boolean;
		/** Whether the server runs crossplay, which decides the form of id `kick` matches. */
		crossplay?: boolean;
		/** Called after an id was added to one of the lists. */
		onadded: (kind: PlayerListKind) => void;
	} = $props();

	const lists: Record<
		PlayerListKind,
		{ verb: string; title: string; effect: string; icon: typeof Ban }
	> = {
		bans: {
			verb: 'Ban',
			title: 'Banned',
			effect: 'They are refused when they try to join.',
			icon: Ban
		},
		admins: {
			icon: ShieldPlus,
			verb: 'Make admin',
			title: 'Admins',
			effect: 'They may use in-game administrator commands.'
		},
		permitted: {
			icon: UserCheck,
			verb: 'Allow',
			title: 'Allowlist',
			effect:
				'If the allowlist is empty, this turns it on, and only players on it may join this server.'
		}
	};
	const kinds = Object.keys(lists) as PlayerListKind[];

	type Pending = { player: SeenPlayer; kind: PlayerListKind } | { player: SeenPlayer; kick: true };

	let pending = $state<Pending | null>(null);
	let open = $state(false);
	let busy = $state(false);
	// The outcome of the last action, shown on the row it was taken on.
	let outcome = $state<{ id: string; text: string; failure: unknown } | null>(null);

	function label(player: SeenPlayer): string {
		const name = player.names.find((n) => n.likely)?.name ?? player.name;
		return name ? `${name} (${player.platform_id})` : player.platform_id;
	}

	function ask(next: Pending) {
		pending = next;
		open = true;
	}

	async function run() {
		const action = pending;
		open = false;
		if (!action) return;
		const id = action.player.platform_id;
		busy = true;
		outcome = null;
		try {
			if ('kick' in action) {
				// The game matches a connection's host name: the stored id on crossplay, the bare
				// SteamID on a Steam-only server.
				const target = crossplay ? id : id.replace(/^Steam_/, '');
				const result = await instances.command(instanceId, `kick ${target}`);
				outcome = { id, text: `The server answered: ${result.output}`, failure: null };
			} else {
				const list = lists[action.kind].title;
				const added = (await addToPlayerList(instanceId, action.kind, id)) === 'added';
				outcome = {
					id,
					text: added ? `Added to ${list}.` : `Already on ${list}.`,
					failure: null
				};
				onadded(action.kind);
			}
		} catch (err) {
			outcome = { id, text: '', failure: err };
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Seen on this server</Card.Title>
		<Card.Description>
			Accounts this server named in its log. Seen means the account reached this server, not that it
			played or was let in.
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
			<ul class="grid gap-3">
				{#each players as player (player.platform_id)}
					<li class="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
						<span class="grid gap-0.5">
							<span class="flex flex-wrap items-baseline gap-2">
								{#if player.names.length > 0}
									<PlayerNames names={player.names} />
								{:else if player.name}
									<span class="text-sm">{player.name}</span>
								{/if}
								<span class="font-mono text-xs break-all">{player.platform_id}</span>
							</span>
							<span class="text-xs text-muted-foreground"
								>last seen {formatInstant(player.last_seen_at)}</span
							>
							{#if player.wrong_password_at}
								<span class="text-xs text-destructive"
									>Refused: wrong password, {formatInstant(player.wrong_password_at)}</span
								>
							{/if}
							{#if outcome?.id === player.platform_id && outcome.text}
								<span class="text-xs text-muted-foreground" role="status">{outcome.text}</span>
							{/if}
						</span>
						<span class="flex flex-wrap gap-1">
							{#each kinds as kind (kind)}
								<Button
									variant="ghost"
									size="sm"
									disabled={busy}
									aria-label="{lists[kind].verb} {player.platform_id}"
									onclick={() => ask({ player, kind })}
								>
									{@const Icon = lists[kind].icon}
									<Icon />
									<span>{lists[kind].verb}</span>
								</Button>
							{/each}
							{#if canKick}
								<Button
									variant="ghost"
									size="sm"
									disabled={busy}
									aria-label="Kick {player.platform_id}"
									onclick={() => ask({ player, kick: true })}
								>
									<UserX />
									<span>Kick</span>
								</Button>
							{/if}
							<CopyButton
								value={player.platform_id}
								label="Copy ID"
								variant="ghost"
								size="sm"
								ariaLabel="Copy {player.platform_id}"
							/>
						</span>
						{#if outcome?.id === player.platform_id && outcome.failure}
							<div class="basis-full"><Problem error={outcome.failure} /></div>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</Card.Content>
</Card.Root>

<Dialog.Root bind:open>
	<Dialog.Content>
		{#if pending}
			<Dialog.Header>
				{#if 'kick' in pending}
					<Dialog.Title>Kick {label(pending.player)}?</Dialog.Title>
					<Dialog.Description>
						Asks the server to disconnect the account if it is connected; it can join again unless
						it is banned. The server answers “Kicked” whether or not it found the account, so check
						the player count afterwards.
					</Dialog.Description>
				{:else}
					<Dialog.Title>{lists[pending.kind].verb} {label(pending.player)}?</Dialog.Title>
					<Dialog.Description>
						Adds the ID to {lists[pending.kind].title}. {lists[pending.kind].effect}
					</Dialog.Description>
				{/if}
			</Dialog.Header>
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (open = false)}>Cancel</Button>
				<Button onclick={() => void run()}>
					{'kick' in pending ? 'Kick' : lists[pending.kind].verb}
				</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
