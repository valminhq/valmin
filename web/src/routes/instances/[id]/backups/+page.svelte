<script lang="ts">
	import { page } from '$app/state';
	import { instances, type Instance } from '$lib/api/instances';
	import BackupsPanel from '$lib/components/backups-panel.svelte';
	import Problem from '$lib/components/problem.svelte';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);

	$effect(() => {
		void load(id);
	});

	/** Re-read after a job, because a restore changes the world on disk and retention changes
	 * the row the panel renders its counts from. */
	async function load(instanceId: string) {
		try {
			instance = await instances.get(instanceId);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}
</script>

<div class="mx-auto grid max-w-7xl gap-6 p-6">
	<header class="grid gap-3">
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h2 class="text-2xl font-semibold tracking-tight">Backups</h2>
				<p class="text-sm text-muted-foreground">
					What this server has archived, what runs on its own, and how much is kept.
				</p>
			</div>
		</div>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else}
		<BackupsPanel {instance} onchange={() => load(id)} />
	{/if}
</div>
