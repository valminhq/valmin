<script lang="ts">
	import { resolve } from '$app/paths';
	import { diagnostics, type DiagnosticCheck, type DiagnosticsReport } from '$lib/api/admin';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import * as Card from '$lib/components/ui/card';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Download from '@lucide/svelte/icons/download';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';

	let report = $state<DiagnosticsReport | null>(null);
	let loading = $state(true);
	let jobId = $state<string | null>(null);
	let deepRunning = $state(false);
	let failure = $state<unknown>(null);

	// Rendered from allowed_actions, never from a role name (F3).
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));

	// Grouped in first-seen order. An array rather than a Map: the backend orders the checks
	// and a handful of groups do not need an index.
	const groups = $derived.by(() => {
		const out: Array<[string, DiagnosticCheck[]]> = [];
		for (const check of report?.checks ?? []) {
			const bucket = out.find(([group]) => group === check.group);
			if (bucket) bucket[1].push(check);
			else out.push([check.group, [check]]);
		}
		return out;
	});

	const counts = $derived.by(() => {
		const tally = { ok: 0, warn: 0, fail: 0, unknown: 0 };
		for (const check of report?.checks ?? []) tally[check.status]++;
		return tally;
	});

	async function load() {
		failure = null;
		loading = true;
		try {
			report = await diagnostics.read();
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function runDeep() {
		failure = null;
		deepRunning = true;
		try {
			jobId = (await diagnostics.run()).job_id;
		} catch (err) {
			failure = err;
			deepRunning = false;
		}
	}

	function onDeepFinished() {
		deepRunning = false;
		void load();
	}

	function badge(status: DiagnosticCheck['status']) {
		if (status === 'fail') return 'destructive';
		if (status === 'warn') return 'secondary';
		return 'outline';
	}

	function when(at: string | undefined) {
		return at ? new Date(at).toLocaleString() : 'never';
	}

	$effect(() => {
		if (allowed) void load();
		else loading = false;
	});
</script>

<main class="mx-auto grid max-w-5xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Diagnostics</h1>
		<p class="text-sm text-muted-foreground">
			What this panel can and cannot reach, and the settings that decide whether it works. Every row
			says where its answer came from.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">Diagnostics are not available to you.</p>
	{:else}
		<div class="flex flex-wrap items-center gap-3">
			<Button variant="outline" size="sm" onclick={load} disabled={loading}>
				<RefreshCw /> Refresh
			</Button>
			<Button variant="outline" size="sm" onclick={runDeep} disabled={deepRunning}>
				<RefreshCw /> Run deep checks
			</Button>
			<Button variant="outline" size="sm" href={diagnostics.bundleUrl()}>
				<Download /> Download support bundle
			</Button>
			{#if report}
				<span class="text-sm text-muted-foreground">
					{counts.ok} passing, {counts.warn} warnings, {counts.fail} failing, {counts.unknown} not measured
				</span>
			{/if}
		</div>

		<p class="text-sm text-muted-foreground">
			The deep checks start a throwaway container each to re-prove the host data root, the game
			network and Steam, so they run as a job rather than on page load.
		</p>

		{#if jobId}
			<JobProgress {jobId} onfinish={onDeepFinished} />
		{/if}

		{#if loading && !report}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if report}
			{#each groups as [group, checks] (group)}
				<Card.Root>
					<Card.Header>
						<Card.Title>{group}</Card.Title>
					</Card.Header>
					<Card.Content class="grid gap-3">
						{#each checks as check (check.id)}
							<div class="grid gap-1 rounded-lg border p-3">
								<div class="flex flex-wrap items-center gap-2">
									<Badge variant={badge(check.status)}>{check.status}</Badge>
									<span class="text-sm font-medium">{check.title}</span>
									<span class="ml-auto text-xs text-muted-foreground">
										{check.source} · {when(check.measured_at)}
									</span>
								</div>
								<p class="text-sm">{check.detail}</p>
								{#if check.diagnostic}
									<pre
										class="overflow-x-auto rounded bg-muted p-2 text-xs whitespace-pre-wrap">{check.diagnostic}</pre>
								{/if}
								{#if check.remedy}
									<p class="text-sm text-muted-foreground">{check.remedy}</p>
								{/if}
							</div>
						{/each}
					</Card.Content>
				</Card.Root>
			{/each}

			<Card.Root>
				<Card.Header>
					<Card.Title>Servers</Card.Title>
					<Card.Description>
						UDP ports as allocated against what the container engine publishes. This does not test
						reachability from the internet.
					</Card.Description>
				</Card.Header>
				<Card.Content>
					{#if report.instances.length === 0}
						<p class="text-sm text-muted-foreground">No servers yet.</p>
					{:else}
						<div class="overflow-x-auto">
							<table class="w-full text-left text-sm">
								<thead class="text-xs text-muted-foreground">
									<tr>
										<th class="py-2 pr-4 font-medium">Server</th>
										<th class="py-2 pr-4 font-medium">State</th>
										<th class="py-2 pr-4 font-medium">Allocated</th>
										<th class="py-2 pr-4 font-medium">Published</th>
										<th class="py-2 pr-4 font-medium">Mods</th>
										<th class="py-2 pr-4 font-medium">Console</th>
									</tr>
								</thead>
								<tbody>
									{#each report.instances as row (row.id)}
										<tr class="border-t align-top">
											<td class="py-2 pr-4">{row.name}</td>
											<td class="py-2 pr-4">
												{row.state}
												{#if row.restart_required}
													<Badge variant="secondary">restart required</Badge>
												{/if}
											</td>
											<td class="py-2 pr-4 tabular-nums">{row.expected_ports.join(', ')}</td>
											<td class="py-2 pr-4 tabular-nums">
												{row.bound_ports?.length ? row.bound_ports.join(', ') : '—'}
												{#if row.port_issue}
													<span class="text-destructive">{row.port_issue}</span>
												{/if}
											</td>
											<td class="py-2 pr-4 tabular-nums">{row.mods}</td>
											<td class="py-2 pr-4">
												{row.running
													? row.log_reader_attached
														? 'attached'
														: 'not attached'
													: '—'}
											</td>
										</tr>
									{/each}
								</tbody>
							</table>
						</div>
					{/if}
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Build</Card.Title>
				</Card.Header>
				<Card.Content class="grid gap-1 font-mono text-xs">
					<span
						>{report.build.version} ({report.build.commit || 'no commit'}) {report.build.go}</span
					>
					<span class="text-muted-foreground">
						started {when(report.started_at)} · {report.migrations.length} migrations applied
					</span>
				</Card.Content>
			</Card.Root>
		{/if}
	{/if}
</main>
