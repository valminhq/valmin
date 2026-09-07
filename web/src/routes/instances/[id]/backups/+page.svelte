<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { instances, type Instance } from '$lib/api/instances';
	import { Button } from '$lib/components/ui/button';
	import BackupsPanel from '$lib/components/backups-panel.svelte';
	import Problem from '$lib/components/problem.svelte';
	import SchedulesEditor from '$lib/components/schedules-editor.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';

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

<div class="mx-auto grid max-w-3xl gap-6 p-6">
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
				<h1 class="text-lg font-semibold">Backups</h1>
				<p class="text-sm text-muted-foreground">
					What this server has archived, what runs on its own, and how much is kept.
				</p>
			</div>
			{#if instance}
				<StateBadge state={instance.state} restartRequired={instance.restart_required} />
			{/if}
		</div>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else}
		<BackupsPanel {instance} onchange={() => load(id)} />
		<SchedulesEditor {instance} />
	{/if}
</div>
