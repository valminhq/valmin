<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { actions, instances, isTransient, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import { socketStatus } from '$lib/socket/index.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import Problem from '$lib/components/problem.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import LogOut from '@lucide/svelte/icons/log-out';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import Plus from '@lucide/svelte/icons/plus';

	let failure = $state<unknown>(null);
	let busy = $state<string | null>(null);
	let confirming = $state<Instance | null>(null);
	let confirmOpen = $state(false);

	$effect(() => {
		void instanceList.load();
	});

	// A reconnect re-reads the list: the socket cannot say what changed while it was gone
	// (`14 §7.2`), and the state topics were re-subscribed from scratch (ADR-041).
	let lastStatus = $state(socketStatus.value);
	$effect(() => {
		const status = socketStatus.value;
		if (status === 'open' && lastStatus !== 'open') void instanceList.load();
		lastStatus = status;
	});

	// `↯` F3: rendered from allowed_actions, never from a role name — including this one,
	// which is why /me/permissions carries a *global* action list at all. Client-side hiding
	// is cosmetic; the server checks every request regardless, so this decides what is
	// *shown* and nothing else.
	const canCreate = $derived(session.allowedGlobally().includes(actions.create));

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

	async function signOut() {
		try {
			await api.post('/auth/logout');
		} finally {
			instanceList.release();
			session.signedOut();
			await goto(resolve('/login'));
		}
	}
</script>

<div class="min-h-screen">
	<header class="flex items-center justify-between border-b px-6 py-3">
		<span class="font-semibold">Valmin</span>
		<div class="flex items-center gap-3">
			{#if socketStatus.value !== 'open'}
				<span class="text-xs text-muted-foreground">
					{socketStatus.value === 'connecting' ? 'reconnecting…' : 'offline'}
				</span>
			{/if}
			<span class="text-sm text-muted-foreground">{session.user?.username ?? ''}</span>
			<Button variant="ghost" size="icon-sm" onclick={signOut} aria-label="Sign out">
				<LogOut />
			</Button>
		</div>
	</header>

	<main class="mx-auto grid max-w-4xl gap-4 p-6">
		<div class="flex items-center justify-between">
			<h1 class="text-lg font-semibold">Servers</h1>
			{#if canCreate}
				<Button href={resolve('/instances/new')}>
					<Plus />
					New server
				</Button>
			{/if}
		</div>

		<Problem error={failure ?? instanceList.error} />

		{#if instanceList.loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
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
						<Card.Title class="flex items-center justify-between gap-3">
							<span>{instance.name}</span>
							<StateBadge state={instance.state} restartRequired={instance.restart_required} />
						</Card.Title>
						<Card.Description>
							{instance.server_name} · world {instance.world_name} · udp {instance.base_port}–{instance.base_port +
								1}
						</Card.Description>
					</Card.Header>
					<Card.Footer class="flex gap-2">
						{#if allowed.includes(actions.start)}
							<Button
								variant="outline"
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
		description="The container and this server's settings are removed. Its worlds are kept on disk — nothing here deletes a world."
		onconfirm={() => run(target, () => instances.remove(target.id, true))}
	/>
{/if}
