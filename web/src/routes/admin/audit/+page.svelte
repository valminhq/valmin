<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { auditLog, type AuditEntry, type AuditFilter, type AuditFilters } from '$lib/api/admin';
	import {
		actorName,
		changes,
		dayRange,
		describe,
		label,
		outcomeLabel,
		outcomeVariant,
		serverName
	} from '$lib/audit';
	import { instanceList } from '$lib/state/instances.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import * as Select from '$lib/components/ui/select';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Download from '@lucide/svelte/icons/download';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';

	const any = 'any';

	// The filters live in the address, so a filtered view can be linked and survives a reload.
	const action = $derived(page.url.searchParams.get('action') ?? '');
	const userId = $derived(page.url.searchParams.get('user_id') ?? '');
	const instanceId = $derived(page.url.searchParams.get('instance_id') ?? '');
	const from = $derived(page.url.searchParams.get('from') ?? '');
	const to = $derived(page.url.searchParams.get('to') ?? '');
	const filter: AuditFilter = $derived({
		action: action || undefined,
		user_id: userId || undefined,
		instance_id: instanceId || undefined,
		...dayRange(from, to)
	});
	const filtered = $derived(Object.values(filter).some(Boolean));

	let entries = $state.raw<AuditEntry[]>([]);
	let cursor = $state<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<unknown>(null);
	let facets = $state.raw<AuditFilters | null>(null);
	let facetFailure = $state<unknown>(null);

	const actionOptions = $derived(
		(facets?.actions ?? [])
			.map((value) => ({ value, text: label(value) }))
			.sort((a, b) => a.text.localeCompare(b.text))
	);
	const existing = $derived(new Set(instanceList.items.map((i) => i.id)));
	const listed = $derived(!instanceList.loading && !instanceList.error);

	$effect(() => {
		void loadFacets();
		void instanceList.ensure();
	});

	// Re-runs whenever a filter changes, which also discards the old page's cursor.
	$effect(() => {
		void reload(filter);
	});

	async function loadFacets() {
		try {
			facets = await auditLog.filters();
			facetFailure = null;
		} catch (err) {
			facetFailure = err;
		}
	}

	// A response is applied only if no newer load has started since its request left. Starting
	// one aborts the previous request, so a slow older answer cannot overwrite newer filters.
	let generation = 0;
	let controller = new AbortController();

	async function reload(next: AuditFilter) {
		const mine = ++generation;
		controller.abort();
		controller = new AbortController();
		loading = true;
		loadingMore = false;
		failure = null;
		try {
			const result = await auditLog.list(next, undefined, controller.signal);
			if (mine !== generation) return;
			entries = result.items;
			cursor = result.next_cursor;
		} catch (err) {
			if (mine === generation) failure = err;
		} finally {
			if (mine === generation) loading = false;
		}
	}

	async function loadMore() {
		if (!cursor || loadingMore) return;
		const mine = generation;
		loadingMore = true;
		try {
			const result = await auditLog.list(filter, cursor, controller.signal);
			if (mine !== generation) return;
			entries = [...entries, ...result.items];
			cursor = result.next_cursor;
		} catch (err) {
			if (mine === generation) failure = err;
		} finally {
			if (mine === generation) loadingMore = false;
		}
	}

	function refresh() {
		void reload(filter);
		void loadFacets();
	}

	function setFilter(key: string, value: string) {
		const url = new URL(page.url);
		if (value && value !== any) url.searchParams.set(key, value);
		else url.searchParams.delete(key);
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		void goto(url, { replaceState: true, keepFocus: true, noScroll: true });
	}

	function when(at: string): string {
		return new Date(at).toLocaleString();
	}
</script>

<main class="mx-auto grid max-w-5xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Audit log</h1>
		<p class="text-sm text-muted-foreground">
			Actions taken through the panel: who did what, to which server, and how it ended. Never
			pruned; survives the users and servers it describes.
		</p>
	</header>

	<Problem error={failure} />
	<Problem error={facetFailure} />

	<section class="grid gap-4 sm:grid-cols-3">
		<div class="grid gap-2">
			<Label for="filter-action">Action</Label>
			<Select.Root
				type="single"
				value={action || any}
				onValueChange={(value) => setFilter('action', value)}
			>
				<Select.Trigger id="filter-action">{action ? label(action) : 'Any action'}</Select.Trigger>
				<Select.Content>
					<Select.Item value={any}>Any action</Select.Item>
					{#each actionOptions as option (option.value)}
						<Select.Item value={option.value}>{option.text}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
		<div class="grid gap-2">
			<Label for="filter-user">Actor</Label>
			<Select.Root
				type="single"
				value={userId || any}
				onValueChange={(value) => setFilter('user_id', value)}
			>
				<Select.Trigger id="filter-user">
					{userId ? (facets?.actors.find((a) => a.id === userId)?.name ?? userId) : 'Anyone'}
				</Select.Trigger>
				<Select.Content>
					<Select.Item value={any}>Anyone</Select.Item>
					{#each facets?.actors ?? [] as actor (actor.id)}
						<Select.Item value={actor.id}>{actor.name || actor.id}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
		<div class="grid gap-2">
			<Label for="filter-instance">Server</Label>
			<Select.Root
				type="single"
				value={instanceId || any}
				onValueChange={(value) => setFilter('instance_id', value)}
			>
				<Select.Trigger id="filter-instance">
					{instanceId
						? (facets?.instances.find((s) => s.id === instanceId)?.name ?? instanceId)
						: 'Any server'}
				</Select.Trigger>
				<Select.Content>
					<Select.Item value={any}>Any server</Select.Item>
					{#each facets?.instances ?? [] as server (server.id)}
						<Select.Item value={server.id}>{server.name || server.id}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
		<div class="grid gap-2">
			<Label for="filter-from">From (your date)</Label>
			<Input
				id="filter-from"
				type="date"
				value={from}
				max={to || undefined}
				onchange={(e) => setFilter('from', e.currentTarget.value)}
			/>
		</div>
		<div class="grid gap-2">
			<Label for="filter-to">To (your date)</Label>
			<Input
				id="filter-to"
				type="date"
				value={to}
				min={from || undefined}
				onchange={(e) => setFilter('to', e.currentTarget.value)}
			/>
		</div>
		<div class="flex items-end gap-2">
			<Button variant="outline" onclick={refresh}><RefreshCw /> Refresh</Button>
			<Button variant="outline" href={auditLog.exportUrl(filter)} download>
				<Download /> Export CSV
			</Button>
		</div>
	</section>

	{#if entries.length === 0 && !loading}
		<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
			{filtered ? 'No entries match these filters.' : 'Nothing has been recorded yet.'}
		</div>
	{/if}

	<div class="overflow-x-auto">
		<table class="w-full text-left text-sm">
			<thead class="text-xs text-muted-foreground">
				<tr>
					<th class="py-2 pr-4 font-medium">When</th>
					<th class="py-2 pr-4 font-medium">Actor</th>
					<th class="py-2 pr-4 font-medium">What happened</th>
					<th class="py-2 pr-4 font-medium">Outcome</th>
					<th class="py-2 font-medium">Server</th>
				</tr>
			</thead>
			<tbody>
				{#each entries as entry (entry.id)}
					{@const outcome = outcomeLabel(entry.outcome)}
					{@const rows = changes(entry.detail)}
					<tr class="border-t align-top">
						<td class="py-2 pr-4 whitespace-nowrap tabular-nums">{when(entry.created_at)}</td>
						<td class="py-2 pr-4">{actorName(entry)}</td>
						<td class="py-2 pr-4">
							<details>
								<summary class="cursor-pointer">{describe(entry)}</summary>
								<dl class="mt-2 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
									<dt class="text-muted-foreground">Action</dt>
									<dd class="font-mono">{entry.action}</dd>
									{#if rows.length}
										<dt class="text-muted-foreground">Changes</dt>
										<dd>
											<ul class="grid gap-0.5">
												{#each rows as row, i (i)}
													<li>
														<span class="font-mono">{row.field}</span>{row.secret
															? ' changed (value not recorded)'
															: `: ${row.from} → ${row.to}`}
													</li>
												{/each}
											</ul>
										</dd>
									{/if}
									{#if entry.job_id}
										<dt class="text-muted-foreground">Job</dt>
										<dd><span class="font-mono break-all">{entry.job_id}</span></dd>
										<dt class="text-muted-foreground">Job status</dt>
										<dd>{outcome ?? 'Unknown'}</dd>
										{#if entry.job_error}
											<dt class="text-muted-foreground">Job error</dt>
											<dd class="break-words">{entry.job_error}</dd>
										{/if}
									{/if}
									<dt class="text-muted-foreground">Actor ID</dt>
									<dd class="font-mono break-all">{entry.user_id ?? '—'}</dd>
									<dt class="text-muted-foreground">Server ID</dt>
									<dd class="font-mono break-all">{entry.instance_id ?? '—'}</dd>
									<dt class="text-muted-foreground">IP</dt>
									<dd class="font-mono">{entry.ip ?? '—'}</dd>
									<dt class="text-muted-foreground">Entry ID</dt>
									<dd class="font-mono break-all">{entry.id}</dd>
									<dt class="text-muted-foreground">Detail</dt>
									<dd><code class="break-all">{entry.detail ?? '—'}</code></dd>
								</dl>
							</details>
						</td>
						<td class="py-2 pr-4">
							{#if outcome}
								<Badge variant={outcomeVariant(entry.outcome)}>{outcome}</Badge>
							{/if}
						</td>
						<td class="py-2">
							{#if entry.instance && entry.instance_id && existing.has(entry.instance_id)}
								<a
									class="underline-offset-4 hover:underline"
									href={resolve('/instances/[id]', { id: entry.instance_id })}
								>
									{entry.instance}
								</a>
							{:else if serverName(entry)}
								{serverName(entry)}
								{#if entry.instance && entry.instance_id && listed}
									<span class="text-xs text-muted-foreground">(deleted)</span>
								{/if}
							{:else}
								—
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>

	{#if loading || loadingMore}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if cursor}
		<Button variant="outline" class="justify-self-start" onclick={loadMore}>Load older</Button>
	{/if}
</main>
