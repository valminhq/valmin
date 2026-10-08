<script lang="ts">
	import { formatInstant } from '$lib/api/schedules';
	import { resolve } from '$app/paths';
	import type { Job } from '$lib/api/types';
	import { outcomeLabel } from '$lib/audit';
	import { changeLine, jobActor, jobDuration, jobOutcome, nextAction } from '$lib/job-history';
	import { session } from '$lib/state/session.svelte';

	let { job }: { job: Job } = $props();

	const duration = $derived(jobDuration(job));
	const changes = $derived(job.changes ?? []);
	const succeeded = $derived(job.status === 'succeeded');
	const next = $derived(nextAction(job));
	const nextId = $derived(
		next && job.instance_id && session.can(job.instance_id, next.requires) ? job.instance_id : null
	);
</script>

<div class="grid gap-3 text-sm">
	{#if job.error || job.error_code}
		<div class="grid gap-1">
			{#if job.error}
				<p class="break-words text-destructive">{job.error}</p>
			{/if}
			{#if job.error_code}
				<p class="text-xs text-muted-foreground">
					Code <code class="font-mono">{job.error_code}</code>
				</p>
			{/if}
		</div>
	{:else if job.status === 'failed' && !job.message}
		<p class="text-muted-foreground">No error details recorded.</p>
	{/if}

	<dl class="grid gap-x-6 gap-y-1 sm:grid-cols-[auto_1fr]">
		<dt class="text-muted-foreground">When</dt>
		<dd>{formatInstant(job.created_at)}</dd>
		<dt class="text-muted-foreground">Started by</dt>
		<dd>{jobActor(job)}</dd>
		{#if duration}
			<dt class="text-muted-foreground">Duration</dt>
			<dd>{duration}</dd>
		{/if}
		<dt class="text-muted-foreground">Outcome</dt>
		<dd class="break-words">
			{outcomeLabel(jobOutcome(job.status))}{#if job.message}{' · ' + job.message}{/if}
		</dd>
	</dl>

	{#if changes.length}
		<div class="grid gap-1">
			<p class="text-muted-foreground">{succeeded ? 'Changes' : 'Changes requested'}</p>
			<ul class="grid gap-0.5">
				{#each changes as change (change.full_name)}
					<li class="break-words">{changeLine(change, succeeded)}</li>
				{/each}
			</ul>
		</div>
	{/if}

	{#if next && nextId}
		{#if next.section === 'mods'}
			<a class="underline" href={resolve('/instances/[id]/mods', { id: nextId })}>{next.label}</a>
		{:else if next.section === 'setups'}
			<a class="underline" href={resolve('/instances/[id]/setups', { id: nextId })}>
				{next.label}
			</a>
		{:else if next.section === 'backups'}
			<a class="underline" href={resolve('/instances/[id]/backups', { id: nextId })}>
				{next.label}
			</a>
		{:else}
			<a class="underline" href={resolve(`/instances/[id]#${next.section}`, { id: nextId })}>
				{next.label}
			</a>
		{/if}
	{/if}
</div>
