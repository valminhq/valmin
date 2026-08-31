<script lang="ts">
	import { watchJob } from '$lib/api/jobs';
	import type { Job } from '$lib/api/types';
	import { socket } from '$lib/socket/index.svelte';
	import { Progress } from '$lib/components/ui/progress';

	let { jobId, onfinish }: { jobId: string; onfinish?: (job: Job) => void } = $props();

	let job = $state<Job | null>(null);

	$effect(() => {
		// subscribe-then-fetch is watchJob's, not this component's (`14 §7.2`, `06 §4`): it is
		// written once in the API client so a second screen cannot get the ordering wrong.
		return watchJob(socket, jobId, (next) => {
			job = next;
			if (next.status !== 'queued' && next.status !== 'running') onfinish?.(next);
		});
	});
</script>

<!--
	`↯` F4: optimistic UI is forbidden for anything touching world data. This renders the job
	the daemon actually reports — including the long flat stretch while a ~1 GB clone runs on
	ext4, where `--reflink=auto` degrades silently to a full copy (`08 §3`). A bar that
	animated ahead of the work would be a lie precisely when the operator is deciding whether
	something has hung.
-->
{#if job}
	<div class="grid gap-2">
		<Progress value={job.progress} max={100} />
		<p class="text-sm text-muted-foreground">
			{job.message || job.status}
			{#if job.status === 'running'}<span class="tabular-nums"> · {job.progress}%</span>{/if}
		</p>
		{#if job.error}
			<p class="text-sm text-destructive">{job.error}</p>
		{/if}
	</div>
{:else}
	<p class="text-sm text-muted-foreground">Starting…</p>
{/if}
