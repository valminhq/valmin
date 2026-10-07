<script lang="ts">
	import { instances } from '$lib/api/instances';
	import { inZone, kindLabel, viewerZone } from '$lib/api/schedules';
	import type { Job } from '$lib/api/types';
	import { socket } from '$lib/socket/index.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import * as Card from '$lib/components/ui/card';
	import Problem from '$lib/components/problem.svelte';

	let { instanceId }: { instanceId: string } = $props();

	const recent = 50;
	const viewer = $derived(viewerZone());

	let runs = $state<Job[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);

	const missed = $derived(runs.filter((j) => j.status === 'failed' || j.status === 'cancelled'));

	$effect(() => {
		const id = instanceId;
		const unhook = socket.onConnected(() => void load(id));
		const tick = setInterval(() => void load(id), 60_000);
		void load(id);
		return () => {
			unhook();
			clearInterval(tick);
		};
	});

	/** Reads the latest scheduled runs of this instance, newest first. A reply for an instance
	 * no longer shown is dropped. */
	async function load(id: string) {
		try {
			const rows = await instances.jobs(id, recent, true);
			if (id !== instanceId) return;
			runs = rows;
			failure = null;
		} catch (err) {
			if (id === instanceId) failure = err;
		} finally {
			if (id === instanceId) loading = false;
		}
	}

	/** A cancelled run carrying an error code was skipped by the clock; without one, a person
	 * or a daemon shutdown cancelled it. */
	function outcome(j: Job): { text: string; variant: 'destructive' | 'secondary' | 'outline' } {
		if (j.status === 'failed') return { text: 'Failed', variant: 'destructive' };
		if (j.error_code) return { text: 'Skipped', variant: 'secondary' };
		return { text: 'Cancelled', variant: 'outline' };
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Skipped and failed runs</Card.Title>
		<Card.Description>
			Scheduled runs that did not complete, newest first. Times are shown in {viewer}.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<Problem error={failure} />
		{#if loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if !failure && missed.length === 0}
			<p class="text-sm text-muted-foreground">
				No skipped or failed runs among the last {recent} scheduled runs.
			</p>
		{:else}
			<ul class="grid gap-2">
				{#each missed as run (run.job_id)}
					{@const result = outcome(run)}
					{@const at = run.finished_at ?? run.created_at}
					<li class="grid gap-1 rounded-lg border p-3 text-sm">
						<div class="flex flex-wrap items-center gap-2">
							<span class="font-medium">{kindLabel(run.kind)}</span>
							<Badge variant={result.variant}>{result.text}</Badge>
						</div>
						<span>{inZone(at, viewer)} {viewer}</span>
						{#if run.error}<p>{run.error}</p>{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</Card.Content>
</Card.Root>
