<script lang="ts">
	import {
		shutdowns,
		SHUTDOWN_STOP_MINUTES,
		SHUTDOWN_WARN_MINUTES,
		type PlannedShutdown
	} from '$lib/api/admin';
	import { actions } from '$lib/api/instances';
	import { inZone, viewerZone } from '$lib/api/schedules';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	const allowed = $derived(session.allowedGlobally().includes(actions.schedulesGlobal));
	const zone = $derived(viewerZone());
	let planned = $state<PlannedShutdown[]>([]);
	let when = $state('');
	let loading = $state(true);
	let busy = $state(false);
	let failure = $state<unknown>(null);

	async function load() {
		loading = true;
		try {
			planned = await shutdowns.list();
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

	async function add(event: SubmitEvent) {
		event.preventDefault();
		if (when === '') return;
		busy = true;
		failure = null;
		try {
			await shutdowns.create(when, zone);
			when = '';
			await load();
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	async function cancel(item: PlannedShutdown) {
		const at = inZone(item.power_off_at, zone);
		if (!confirm(`Cancel the power cut at ${at}? Servers will keep running through it.`)) return;
		failure = null;
		try {
			await shutdowns.remove(item.id);
			await load();
		} catch (err) {
			failure = err;
		}
	}

	function author(item: PlannedShutdown): string {
		return item.created_by_username ?? (item.created_by_name || 'a deleted account');
	}
</script>

<main class="mx-auto grid w-full max-w-3xl gap-6 p-4 sm:p-6">
	<h1 class="text-2xl font-semibold">Power cuts</h1>
	{#if !allowed}
		<p>You do not have permission to plan power cuts.</p>
	{:else}
		<p class="text-sm text-muted-foreground">
			Enter the times the electricity goes off. Players online are warned in chat {SHUTDOWN_WARN_MINUTES}
			minutes before, when the server has the RCON mod, and every running server is stopped
			{SHUTDOWN_STOP_MINUTES} minutes before, so its world is saved before the cut. Servers stay stopped
			after the host comes back; start them from the panel or with Discord's <code>/start</code>.
		</p>
		<Problem error={failure} />

		<Card.Root>
			<form onsubmit={add}>
				<Card.Content class="grid gap-4">
					<div class="grid gap-2">
						<Label for="power-off-at">The power goes off at</Label>
						<Input
							id="power-off-at"
							type="datetime-local"
							class="w-auto justify-self-start"
							bind:value={when}
						/>
						<p class="text-xs text-muted-foreground">In {zone}.</p>
					</div>
					<Button type="submit" class="justify-self-start" disabled={busy || when === ''}>
						{busy ? 'Adding…' : 'Add power cut'}
					</Button>
				</Card.Content>
			</form>
		</Card.Root>

		<Card.Root>
			<Card.Header><Card.Title>Planned</Card.Title></Card.Header>
			<Card.Content>
				{#if loading}<p>Loading power cuts…</p>
				{:else if planned.length === 0}
					<p class="text-sm text-muted-foreground">No power cuts are planned.</p>
				{:else}
					<ul class="grid gap-2">
						{#each planned as item (item.id)}
							<li class="flex flex-wrap items-center justify-between gap-2 rounded-md border p-3">
								<div class="grid">
									<span class="font-medium">{inZone(item.power_off_at, zone)}</span>
									<span class="text-xs text-muted-foreground">Added by {author(item)}</span>
								</div>
								<Button variant="outline" size="sm" onclick={() => cancel(item)}>
									<Trash2 class="size-4" /> Cancel
								</Button>
							</li>
						{/each}
					</ul>
				{/if}
			</Card.Content>
		</Card.Root>
	{/if}
</main>
