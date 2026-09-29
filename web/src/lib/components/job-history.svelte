<script lang="ts">
	import { resolve } from '$app/paths';
	import { actions } from '$lib/api/instances';
	import type { Job } from '$lib/api/types';
	import { outcomeLabel, outcomeVariant } from '$lib/audit';
	import { ago, jobLabel, jobOutcome } from '$lib/job-history';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import JobDetail from '$lib/components/job-detail.svelte';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let {
		instanceId,
		jobs,
		more = false,
		loadingMore = false,
		onmore
	}: {
		instanceId: string;
		/** Newest first. */
		jobs: Job[];
		/** Whether older operations are left to fetch. */
		more?: boolean;
		loadingMore?: boolean;
		onmore?: () => void;
	} = $props();

	const canAudit = $derived(session.allowedGlobally().includes(actions.auditRead));
	const auditHref = $derived(
		`${resolve('/admin/audit')}?instance_id=${encodeURIComponent(instanceId)}`
	);
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Operations</Card.Title>
		<Card.Description>
			{jobs.length ? 'Newest first. Open one for its details.' : 'Nothing has run yet.'}
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-3">
		{#if jobs.length}
			<ul class="grid gap-2">
				{#each jobs as job (job.job_id)}
					{@const failed = job.status === 'failed'}
					{@const outcome = jobOutcome(job.status)}
					<li class="rounded-md border {failed ? 'border-destructive/40 bg-destructive/5' : ''}">
						<details class="group">
							<summary class="flex cursor-pointer flex-wrap items-center gap-2 px-3 py-2">
								<ChevronRight
									class="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-90"
									aria-hidden="true"
								/>
								{#if failed}
									<TriangleAlert class="size-4 text-destructive" aria-hidden="true" />
								{/if}
								<span class="font-medium {failed ? 'text-destructive' : ''}">
									{jobLabel(job.kind, job.changes)}
								</span>
								<Badge variant={outcomeVariant(outcome)}>{outcomeLabel(outcome)}</Badge>
								<span
									class="ml-auto text-xs text-muted-foreground"
									title={new Date(job.created_at).toLocaleString()}
								>
									{ago(job.created_at, Date.now())}
								</span>
							</summary>
							<div class="border-t px-3 py-3">
								<JobDetail {job} />
							</div>
						</details>
					</li>
				{/each}
			</ul>
		{/if}

		{#if more || canAudit}
			<div class="flex flex-wrap items-center gap-3">
				{#if more}
					<Button variant="outline" size="sm" disabled={loadingMore} onclick={onmore}>
						{loadingMore ? 'Loading…' : 'Show older operations'}
					</Button>
				{/if}
				{#if canAudit}
					<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
					<a class="text-sm underline" href={auditHref}>View in the audit log</a>
				{/if}
			</div>
		{/if}
	</Card.Content>
</Card.Root>
