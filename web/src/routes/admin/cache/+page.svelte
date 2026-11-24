<script lang="ts">
	import PageHeader from '#lib/components/page-header.svelte';
	import { pageWide } from '#lib/layout.js';
	import { caches, type CacheEntry, type CacheInventory, type CacheKind } from '#lib/api/admin.js';
	import { actions } from '#lib/api/instances.js';
	import { formatInstant } from '#lib/api/schedules.js';
	import { formatBytes } from '#lib/format.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import EmptyState from '#lib/components/empty-state.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import Problem from '#lib/components/problem.svelte';
	import Eraser from '@lucide/svelte/icons/eraser';

	const KIND_LABELS: Record<CacheKind, string> = {
		mod_archive: 'Mod archives',
		game_build: 'Game builds',
		setup_blob: 'Saved setup files',
		partial_download: 'Unfinished downloads'
	};

	let inventory = $state<CacheInventory | null>(null);
	let unusedOnly = $state(true);
	let confirming = $state(false);
	let jobId = $state<string | null>(null);
	let running = $state(false);
	let failure = $state<unknown>(null);

	// Rendered from allowed_actions, never from a role name. The daemon answers 404 to a caller
	// without it, so hiding the page's controls matches what the endpoint reports.
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));
	const totalBytes = $derived(inventory?.summary.reduce((n, s) => n + s.size_bytes, 0) ?? 0);
	const reclaimableBytes = $derived(
		inventory?.summary.reduce((n, s) => n + s.reclaimable_bytes, 0) ?? 0
	);
	const anyPrunable = $derived(inventory?.items.some((e) => e.prunable) ?? false);
	const graceMinutes = $derived(Math.round((inventory?.grace_seconds ?? 3600) / 60));
	// What a prune would free first, then the largest of the rest.
	const rows = $derived(
		(inventory?.items ?? [])
			.filter((e) => !unusedOnly || !e.in_use)
			.sort((a, b) => b.reclaimable_bytes - a.reclaimable_bytes || b.size_bytes - a.size_bytes)
	);

	async function load() {
		try {
			inventory = await caches.inventory();
		} catch (err) {
			failure = err;
		}
	}

	$effect(() => {
		if (allowed) void load();
	});

	async function prune() {
		failure = null;
		running = true;
		try {
			jobId = (await caches.prune()).job_id;
		} catch (err) {
			failure = err;
			running = false;
		}
	}

	function finished() {
		running = false;
		void load();
	}

	/** Who keeps an entry, or why an unused one is still here. */
	function status(e: CacheEntry): string {
		if (e.in_use) {
			const users = e.servers.map((s) => s.name);
			if (e.setups > 0) users.push(e.setups === 1 ? '1 saved setup' : `${e.setups} saved setups`);
			return `Used by ${users.join(', ')}`;
		}
		if (!e.prunable) {
			return `Recently used, can go after ${formatInstant(e.prunable_at ?? '')}`;
		}
		if (e.reclaimable_bytes > 0) return `Unused, frees ${formatBytes(e.reclaimable_bytes)}`;
		return 'Unused; its bytes are shared with another item and counted there';
	}
</script>

<main class={pageWide}>
	<PageHeader title="Cache">
		Mod archives, game builds and saved setup files the panel keeps on disk. Pruning removes what no
		server and no saved setup uses; it is downloaded again if it is ever needed.
	</PageHeader>

	<Problem error={failure} />

	{#if allowed && inventory}
		<Card.Root>
			<Card.Header>
				<Card.Title>{formatBytes(totalBytes)} cached</Card.Title>
				<Card.Description>
					{#if reclaimableBytes > 0}
						Pruning now frees {formatBytes(reclaimableBytes)}.
					{:else if anyPrunable}
						Pruning now removes unused items, but frees no space: their bytes are shared with items
						still kept.
					{:else}
						Nothing to free: everything cached is in use or was used in the last {graceMinutes} minutes.
					{/if}
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<div class="overflow-x-auto">
					<table class="w-full text-left text-sm">
						<thead class="text-xs text-muted-foreground">
							<tr>
								<th class="py-2 pr-4 font-medium">What</th>
								<th class="py-2 pr-4 text-right font-medium">Items</th>
								<th class="py-2 pr-4 text-right font-medium">On disk</th>
								<th class="py-2 text-right font-medium">Pruning frees</th>
							</tr>
						</thead>
						<tbody>
							{#each inventory.summary as s (s.kind)}
								<tr class="border-t">
									<td class="py-2 pr-4">{KIND_LABELS[s.kind]}</td>
									<td class="py-2 pr-4 text-right tabular-nums">{s.count}</td>
									<td class="py-2 pr-4 text-right tabular-nums">{formatBytes(s.size_bytes)}</td>
									<td class="py-2 text-right tabular-nums">{formatBytes(s.reclaimable_bytes)}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
				<p class="text-sm text-muted-foreground">
					Jobs on servers are refused while the prune runs, which takes seconds. Anything used in
					the last {graceMinutes} minutes is kept, so a server being created keeps its game build.
				</p>
				<Button
					class="justify-self-start"
					variant="destructive"
					onclick={() => (confirming = true)}
					disabled={running || !anyPrunable}
				>
					<Eraser /> Prune cache
				</Button>
				{#if jobId}
					<div class="rounded-lg border p-4">
						<JobProgress {jobId} onfinish={finished} />
					</div>
				{/if}
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Cached items</Card.Title>
				<Card.Description>
					A game build is in use while a server runs it; a mod archive while a server has the mod
					installed or a saved setup holds that version.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<div class="flex items-center gap-3">
					<Switch id="cache-unused-only" bind:checked={unusedOnly} />
					<Label for="cache-unused-only">Only unused items</Label>
				</div>
				{#if rows.length === 0}
					<EmptyState>
						{unusedOnly ? 'Every cached item is in use.' : 'The cache is empty.'}
					</EmptyState>
				{:else}
					<div class="overflow-x-auto">
						<table class="w-full text-left text-sm">
							<thead class="text-xs text-muted-foreground">
								<tr>
									<th class="py-2 pr-4 font-medium">Item</th>
									<th class="py-2 pr-4 text-right font-medium">On disk</th>
									<th class="py-2 font-medium">Status</th>
								</tr>
							</thead>
							<tbody>
								{#each rows as e (`${e.kind}/${e.registry ?? ''}/${e.name}`)}
									<tr class="border-t align-top">
										<td class="py-2 pr-4">
											<span class="font-medium break-all">{e.name}</span>
											<span class="block text-xs text-muted-foreground">
												{KIND_LABELS[e.kind]}{e.registry ? `, ${e.registry}` : ''}
											</span>
										</td>
										<td class="py-2 pr-4 text-right whitespace-nowrap tabular-nums">
											{formatBytes(e.size_bytes)}
										</td>
										<td class="py-2" class:text-muted-foreground={!e.prunable}>
											{status(e)}
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{/if}
			</Card.Content>
		</Card.Root>
	{/if}
</main>

<DestructiveConfirm
	bind:open={confirming}
	title="Prune the cache?"
	description={`This removes the mod archives, game builds and saved setup files nothing uses, freeing ${formatBytes(reclaimableBytes)}. A mod version its registry no longer lists cannot be downloaded again.`}
	confirmLabel="Prune cache"
	onconfirm={prune}
/>
