<script lang="ts">
	import { page } from '$app/state';
	import { instances, type Instance } from '$lib/api/instances';
	import ScheduledRuns from '$lib/components/scheduled-runs.svelte';
	import SchedulesEditor from '$lib/components/schedules-editor.svelte';
	import Problem from '$lib/components/problem.svelte';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);

	$effect(() => {
		void load(id);
	});

	/** Reads the instance, dropping a reply for one no longer shown. */
	async function load(instanceId: string) {
		try {
			const row = await instances.get(instanceId);
			if (instanceId !== id) return;
			instance = row;
			failure = null;
		} catch (err) {
			if (instanceId === id) failure = err;
		} finally {
			if (instanceId === id) loading = false;
		}
	}
</script>

<div class="mx-auto grid max-w-7xl gap-6 p-6">
	<header class="grid gap-1">
		<h2 class="text-2xl font-semibold tracking-tight">Maintenance</h2>
		<p class="text-sm text-muted-foreground">
			Scheduled restarts, backups and game updates for this server, what runs next, and runs that
			did not complete.
		</p>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else}
		<div class="grid items-start gap-6 lg:grid-cols-2">
			<SchedulesEditor {instance} />
			<ScheduledRuns instanceId={instance.id} />
		</div>
	{/if}
</div>
