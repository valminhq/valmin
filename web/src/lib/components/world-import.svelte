<script lang="ts">
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import WorldFilePicker from '$lib/components/world-file-picker.svelte';

	let { instance }: { instance: Instance } = $props();

	let picked = $state<FileList | undefined>();
	let allowBackupVariant = $state(false);
	let confirming = $state(false);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let failure = $state<unknown>(null);

	const files = $derived(Array.from(picked ?? []));
	const allowed = $derived(session.allowed(instance.id));
	const canImport = $derived(allowed.includes(actions.worldImport));

	/**
	 * Why an import cannot start right now, or null when it can. The daemon refuses an import
	 * into a server that is not stopped (C19); this makes the refusal legible before the click.
	 */
	const blocked = $derived.by(() => {
		if (jobRunning) return 'An import is running. Wait for it to finish.';
		if (instance.state === 'running') return 'This server is running. Stop it to import a world.';
		if (instance.state !== 'stopped') {
			return `This server is ${instance.state.replaceAll('_', ' ')}. A world is imported into a stopped server.`;
		}
		return null;
	});
	const ready = $derived(canImport && blocked === null && files.length > 0);

	async function start() {
		failure = null;
		try {
			const job = await instances.importWorld(instance.id, files, allowBackupVariant);
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			failure = err;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Import a world</Card.Title>
		<Card.Description>
			Bring a save from a single-player game or another server. It replaces the world this server
			loads.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		{#if !canImport}
			<p class="text-sm text-muted-foreground" data-testid="import-blocked">
				Importing a world is not available to you.
			</p>
		{:else}
			<WorldFilePicker bind:picked bind:allowBackupVariant disabled={blocked !== null} />

			<Problem error={failure} />

			{#if jobId}
				<!--
					The import archives the existing world first (`03 §4.1` rule 6), which is minutes of
					copying on a large one. Validation failures arrive here too, naming the rule broken
					(F4).
				-->
				<div class="rounded-lg border p-4">
					<JobProgress {jobId} onfinish={() => (jobRunning = false)} />
				</div>
			{/if}

			<div class="flex flex-wrap items-center justify-between gap-3">
				<p class="text-sm text-muted-foreground">
					{blocked ?? 'The world already here is archived first, so it can be restored.'}
				</p>
				<Button
					variant="destructive"
					size="sm"
					disabled={!ready}
					onclick={() => (confirming = true)}
				>
					Import world
				</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>

<!--
	An import overwrites the world this server loads, so the operator types its name back rather
	than dismissing a dialog by reflex (F5).
-->
<DestructiveConfirm
	bind:open={confirming}
	name={instance.world_name}
	title="Replace {instance.world_name}?"
	description="The world this server loads is replaced by the upload, and the panel archives what is there now before it moves anything — it will be in Backups. The imported files are renamed to {instance.world_name}, because that is the world this server starts with."
	confirmLabel="Import"
	onconfirm={start}
/>
