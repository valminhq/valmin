<script lang="ts">
	import PageHeader from '#lib/components/page-header.svelte';
	import { page } from '$app/state';
	import { instances, type Instance } from '#lib/api/instances.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { topics, type ServerMessage } from '#lib/socket/messages.js';
	import { withLive } from '#lib/state/instances.svelte.js';
	import BackupsPanel from '#lib/components/backups-panel.svelte';
	import Problem from '#lib/components/problem.svelte';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);

	$effect(() => {
		const off = socket.subscribe(topics.state(id), (m: ServerMessage) => {
			if (instance) instance = withLive(instance, m);
		});
		void load(id);
		return off;
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

<div class="grid gap-6">
	<PageHeader title="Backups" level={2}>
		What this server has archived, and how much is kept.
	</PageHeader>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else}
		<BackupsPanel {instance} onchange={() => load(id)} />
	{/if}
</div>
