<script lang="ts">
	import type { SetupPreview } from '$lib/api/setups';
	import { modSides, sourceLabel } from '$lib/api/mods';
	import {
		compareConfigs,
		compareMods,
		compareSettings,
		modChanges,
		type ModState
	} from '$lib/compare';
	import { diffLines, hunks } from '$lib/diff';
	import { Badge } from '$lib/components/ui/badge';
	import * as Card from '$lib/components/ui/card';
	import DiffView from '$lib/components/diff-view.svelte';

	let { preview }: { preview: SetupPreview } = $props();
	let opened = $state<Record<string, boolean>>({});

	const settings = $derived(
		compareSettings(preview.current, {
			build: preview.setup.game_build_id,
			launch: preview.setup.instance
		})
	);
	const settingDiffs = $derived(settings.filter((row) => row.change !== 'same'));
	const modRows = $derived(compareMods(preview.current.mods, preview.setup.mods));
	const modDiffs = $derived(modRows.filter((row) => row.change !== 'same'));
	const configRows = $derived(compareConfigs(preview.current.configs, preview.setup.configs));
	const configDiffs = $derived(configRows.filter((row) => row.change !== 'same'));
	const changes = $derived(settingDiffs.length + modDiffs.length + configDiffs.length);
</script>

<div class="grid gap-4">
	<p class="text-sm font-medium">
		{changes === 0
			? 'The saved setup matches the current server.'
			: `Differences: ${settingDiffs.length} in game and settings, ${modDiffs.length} in mods, ${configDiffs.length} in configuration files.`}
	</p>

	<Card.Root>
		<Card.Header>
			<Card.Title>Game and settings</Card.Title>
			<Card.Description>
				Game build and world name are for comparison only. Restoring changes the other settings
				shown here.
			</Card.Description>
		</Card.Header>
		<Card.Content>
			{#if settingDiffs.length === 0}
				<p class="text-sm text-muted-foreground">Game build and settings match.</p>
			{:else}
				<div class="overflow-x-auto">
					<table class="w-full text-left text-sm">
						<thead class="text-xs text-muted-foreground">
							<tr>
								<th scope="col" class="py-2 pr-4 font-medium">Setting</th>
								<th scope="col" class="py-2 pr-4 font-medium">Current server</th>
								<th scope="col" class="py-2 font-medium">Saved setup</th>
							</tr>
						</thead>
						<tbody>
							{#each settingDiffs as row (row.key)}
								<tr class="border-t align-top">
									<th scope="row" class="py-2 pr-4 font-medium">
										{row.label}
										{#if row.key === 'game_build' || row.key === 'world_name'}
											<Badge variant="outline">Compare only</Badge>
										{/if}
									</th>
									<td class="py-2 pr-4 break-all">{row.a}</td>
									<td class="py-2 break-all">{row.b}</td>
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
			<Card.Title>Mods</Card.Title>
			<Card.Description>
				{modRows.length - modDiffs.length} matching; exact versions, registries, enabled state, locks
				and client tags are compared.
			</Card.Description>
		</Card.Header>
		<Card.Content>
			{#if modDiffs.length === 0}
				<p class="text-sm text-muted-foreground">All managed mods match.</p>
			{:else}
				<div class="overflow-x-auto">
					<table class="w-full text-left text-sm">
						<thead class="text-xs text-muted-foreground">
							<tr>
								<th scope="col" class="py-2 pr-4 font-medium">Mod</th>
								<th scope="col" class="py-2 pr-4 font-medium">Current server</th>
								<th scope="col" class="py-2 pr-4 font-medium">Saved setup</th>
								<th scope="col" class="py-2 font-medium">Difference</th>
							</tr>
						</thead>
						<tbody>
							{#each modDiffs as row (row.key)}
								<tr class="border-t align-top">
									<th scope="row" class="py-2 pr-4 font-medium break-all">{row.key}</th>
									<td class="py-2 pr-4">{@render modState(row.a)}</td>
									<td class="py-2 pr-4">{@render modState(row.b)}</td>
									<td class="py-2">
										<div class="flex flex-wrap gap-1">
											{#each modChanges(row, 'current server', 'saved setup') as change (change)}
												<Badge variant="outline">{change}</Badge>
											{/each}
										</div>
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
			<Card.Title>Configuration files</Card.Title>
			<Card.Description>
				{configRows.length - configDiffs.length} matching supported configuration files.
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-3">
			{#if configDiffs.length === 0}
				<p class="text-sm text-muted-foreground">All supported configuration files match.</p>
			{:else}
				{@const onlyOne = configDiffs.filter((row) => row.change !== 'changed')}
				{@const changed = configDiffs.filter((row) => row.change === 'changed')}
				{#if onlyOne.length > 0}
					<ul class="grid gap-1 text-sm">
						{#each onlyOne as row (row.key)}
							<li class="flex flex-wrap items-center gap-2">
								<span class="font-mono break-all">{row.key}</span>
								<Badge variant="outline">Only on {row.a ? 'current server' : 'saved setup'}</Badge>
							</li>
						{/each}
					</ul>
				{/if}
				{#if changed.length > 0}
					<p class="text-sm text-muted-foreground">
						Lines marked − are from the current server; lines marked + are from the saved setup.
					</p>
					{#each changed as row (row.key)}
						<details class="rounded-md border" bind:open={opened[row.key]}>
							<summary class="cursor-pointer px-3 py-2 font-mono text-sm break-all">
								{row.key}
							</summary>
							{#if opened[row.key] && row.a && row.b}
								{@const groups = hunks(diffLines(row.a.content, row.b.content), 2)}
								<div class="px-3 pb-3">
									{#if groups.length > 0}
										<DiffView {groups} />
									{:else}
										<p class="text-sm text-muted-foreground">
											The files differ only in a final line break.
										</p>
									{/if}
								</div>
							{/if}
						</details>
					{/each}
				{/if}
			{/if}
		</Card.Content>
	</Card.Root>
</div>

{#snippet modState(mod: ModState | null)}
	{#if mod}
		<span class="inline-flex flex-wrap items-center gap-2">
			<span class="tabular-nums">{mod.version}</span>
			<span class="text-muted-foreground">{sourceLabel[mod.source] ?? mod.source}</span>
			{#if !mod.enabled}<Badge variant="secondary">disabled</Badge>{/if}
			{#if mod.locked}<Badge variant="secondary">locked</Badge>{/if}
			{#if mod.installed_as === 'dependency'}<Badge variant="outline">dependency</Badge>{/if}
			{#if mod.side && mod.side !== 'unknown'}
				<Badge variant="outline"
					>{modSides.find((side) => side.value === mod.side)?.label ?? mod.side}</Badge
				>
			{/if}
		</span>
	{:else}
		<span class="text-muted-foreground">Not installed</span>
	{/if}
{/snippet}
