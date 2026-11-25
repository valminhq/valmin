<script lang="ts" module>
	import type { ManifestLaunch } from '#lib/api/manifest.js';
	import type { SetupConfig, SetupMod } from '#lib/api/setups.js';

	/** One side of a comparison: the live server or a saved setup. */
	export interface ComparisonSide {
		label: string;
		build: string;
		launch: ManifestLaunch;
		mods: SetupMod[];
		configs: SetupConfig[];
		/** Reads the text of one config file of this side. */
		read: (file: string) => Promise<string>;
	}
</script>

<script lang="ts">
	import { modSides, sourceLabel } from '#lib/api/mods.js';
	import {
		compareMods,
		compareSettings,
		compareSetupConfigs,
		modChanges,
		type ModState,
		type Row
	} from '#lib/compare.js';
	import { diffLines, hunks } from '#lib/diff.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import DiffView from '#lib/components/diff-view.svelte';
	import Problem from '#lib/components/problem.svelte';

	let {
		a,
		b,
		restore = true,
		configTree = true,
		settingsRestored = true,
		readable = true
	}: {
		a: ComparisonSide;
		b: ComparisonSide;
		/** Whether b is what a restore puts in place of a, so differences are worded as its
		 * changes. */
		restore?: boolean;
		/** False when b was saved before setups held every config file. */
		configTree?: boolean;
		/** Whether the rollback restores the launch and backup settings shown. */
		settingsRestored?: boolean;
		/** Whether the viewer may read config file contents; otherwise only names are shown. */
		readable?: boolean;
	} = $props();
	let opened = $state<Record<string, boolean>>({});
	/** Both texts of each changed file opened so far, or why they could not be read. */
	let texts = $state<Record<string, { a: string; b: string } | { failure: unknown }>>({});

	/** Which pair of sides the opened files and loaded texts belong to. */
	let pair = 0;

	// Forgets the opened files and their texts whenever either side is replaced.
	$effect(() => {
		void a;
		void b;
		pair++;
		opened = {};
		texts = {};
	});

	const settings = $derived(
		compareSettings({ build: a.build, launch: a.launch }, { build: b.build, launch: b.launch })
	);
	const settingDiffs = $derived(settings.filter((row) => row.change !== 'same'));
	const modRows = $derived(compareMods(a.mods, b.mods));
	const modDiffs = $derived(modRows.filter((row) => row.change !== 'same'));
	const configRows = $derived(compareSetupConfigs(a.configs, b.configs));
	const configDiffs = $derived(configRows.filter((row) => row.change !== 'same'));
	const removed = $derived(configDiffs.filter((row) => row.change === 'only-a'));
	const added = $derived(configDiffs.filter((row) => row.change === 'only-b'));
	const changed = $derived(configDiffs.filter((row) => row.change === 'changed'));
	const headings = $derived(
		restore
			? {
					removed: 'Removed by the restore',
					added: 'Added by the restore',
					changed: 'Changed by the restore'
				}
			: {
					removed: `Only in ${a.label}`,
					added: `Only in ${b.label}`,
					changed: 'Different'
				}
	);

	/** Loads both texts of a changed file the first time it is opened. */
	async function loadTexts(row: Row<SetupConfig>) {
		if (texts[row.key] || !row.a || !row.b) return;
		const { a: left, b: right } = row;
		const mine = pair;
		try {
			const [first, second] = await Promise.all([
				left.content ?? a.read(row.key),
				right.content ?? b.read(row.key)
			]);
			const lf = (text: string) => text.replaceAll('\r\n', '\n');
			if (mine === pair) texts[row.key] = { a: lf(first), b: lf(second) };
		} catch (err) {
			if (mine === pair) texts[row.key] = { failure: err };
		}
	}
	const changes = $derived(settingDiffs.length + modDiffs.length + configDiffs.length);
</script>

<div class="grid gap-4">
	<p class="text-sm font-medium">
		{changes === 0
			? `${b.label} matches ${restore ? 'the current server' : a.label}.`
			: `Differences: ${settingDiffs.length} in game and settings, ${modDiffs.length} in mods, ${configDiffs.length} in configuration files.`}
	</p>

	<Card.Root>
		<Card.Header>
			<Card.Title>Game and settings</Card.Title>
			<Card.Description>
				{!restore
					? 'The settings each one holds.'
					: settingsRestored
						? 'Game build and world name are for comparison only. Rolling back changes the other settings shown here.'
						: 'For comparison only: this rollback keeps the current settings.'}
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
								<th scope="col" class="py-2 pr-4 font-medium">{a.label}</th>
								<th scope="col" class="py-2 font-medium">{b.label}</th>
							</tr>
						</thead>
						<tbody>
							{#each settingDiffs as row (row.key)}
								<tr class="border-t align-top">
									<th scope="row" class="py-2 pr-4 font-medium">
										{row.label}
										{#if restore && (row.key === 'game_build' || row.key === 'world_name')}
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
								<th scope="col" class="py-2 pr-4 font-medium">{a.label}</th>
								<th scope="col" class="py-2 pr-4 font-medium">{b.label}</th>
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
											{#each modChanges(row, a.label, b.label) as change (change)}
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
				{configRows.length - configDiffs.length} matching configuration files.
				{configTree
					? 'The console password file is not compared.'
					: 'This setup was saved before setups held every config file, so only its top-level .cfg files are compared and restored.'}
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-3">
			{#if configDiffs.length === 0}
				<p class="text-sm text-muted-foreground">All configuration files match.</p>
			{:else}
				{@render fileList(headings.removed, removed)}
				{@render fileList(headings.added, added)}
				{#if changed.length > 0 && !readable}
					{@render fileList(headings.changed, changed)}
				{:else if changed.length > 0}
					<h4 class="text-sm font-medium">{headings.changed} ({changed.length})</h4>
					<p class="text-sm text-muted-foreground">
						Lines marked − are from {a.label}; lines marked + are from {b.label}.
					</p>
					{#each changed as row (row.key)}
						<details
							class="rounded-md border"
							bind:open={opened[row.key]}
							ontoggle={(e) => {
								if (e.currentTarget.open) void loadTexts(row);
							}}
						>
							<summary class="cursor-pointer px-3 py-2 font-mono text-sm break-all">
								{row.key}
							</summary>
							{#if opened[row.key]}
								{@const loaded = texts[row.key]}
								<div class="px-3 pb-3">
									{#if !loaded}
										<p class="text-sm text-muted-foreground">Loading…</p>
									{:else if 'failure' in loaded}
										<Problem error={loaded.failure} />
									{:else}
										{@const groups = hunks(diffLines(loaded.a, loaded.b), 2)}
										{#if groups.length > 0}
											<DiffView {groups} />
										{:else}
											<p class="text-sm text-muted-foreground">
												The files differ only in a final line break.
											</p>
										{/if}
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

{#snippet fileList(title: string, rows: Row<SetupConfig>[])}
	{#if rows.length > 0}
		<div class="grid gap-1">
			<h4 class="text-sm font-medium">{title} ({rows.length})</h4>
			<ul class="grid max-h-64 gap-1 overflow-y-auto font-mono text-sm">
				{#each rows as row (row.key)}
					<li class="break-all">{row.key}</li>
				{/each}
			</ul>
		</div>
	{/if}
{/snippet}

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
