<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { actions, instances } from '$lib/api/instances';
	import { manifest, type ManifestConfig } from '$lib/api/manifest';
	import { mods, sourceLabel, type InstalledMod } from '$lib/api/mods';
	import {
		compareConfigs,
		compareMods,
		compareSettings,
		modChanges,
		type ModState,
		type Settings
	} from '$lib/compare';
	import { diffLines, hunks } from '$lib/diff';
	import { instanceList } from '$lib/state/instances.svelte';
	import { session } from '$lib/state/session.svelte';
	import * as Alert from '$lib/components/ui/alert';
	import { Badge } from '$lib/components/ui/badge';
	import * as Card from '$lib/components/ui/card';
	import { Label } from '$lib/components/ui/label';
	import * as Select from '$lib/components/ui/select';
	import DiffView from '$lib/components/diff-view.svelte';
	import Problem from '$lib/components/problem.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	/** One server as the comparison reads it. */
	interface Side extends Settings {
		name: string;
		mods: InstalledMod[];
		configs: ManifestConfig[];
	}

	const needed = [actions.settings, actions.modsList, actions.configRead];

	const id = $derived(page.params.id ?? '');
	const other = $derived(page.url.searchParams.get('with') ?? '');
	const permitted = (server: string) => needed.every((action) => session.can(server, action));
	const canCompare = $derived(permitted(id));
	const canCompareWith = $derived(other === '' || permitted(other));
	const candidates = $derived(
		instanceList.items.filter((row) => row.id !== id && permitted(row.id))
	);

	let compared = $state<{ a: Side; b: Side } | null>(null);
	let loading = $state(false);
	let failure = $state<unknown>(null);
	let loadRequest = 0;
	/** Which differing files are expanded; a file's diff is computed only while it is open. */
	let opened = $state<Record<string, boolean>>({});

	const settings = $derived(compared ? compareSettings(compared.a, compared.b) : []);
	const modRows = $derived(compared ? compareMods(compared.a.mods, compared.b.mods) : []);
	const configRows = $derived(
		compared ? compareConfigs(compared.a.configs, compared.b.configs) : []
	);
	const settingDiffs = $derived(settings.filter((row) => row.change !== 'same'));
	const modDiffs = $derived(modRows.filter((row) => row.change !== 'same'));
	const configDiffs = $derived(configRows.filter((row) => row.change !== 'same'));
	const buildDiffers = $derived(settings[0]?.change === 'changed');

	const summary = $derived(
		settingDiffs.length + modDiffs.length + configDiffs.length === 0
			? 'The two servers match in game build, settings, mods and configuration files.'
			: `Differences: ${settingDiffs.length} in game and settings, ${modDiffs.length} in mods, ${configDiffs.length} in configuration files.`
	);

	$effect(() => {
		void instanceList.ensure();
	});

	$effect(() => {
		void load(id, other, canCompare && canCompareWith);
	});

	async function load(self: string, peer: string, allowed: boolean) {
		const request = ++loadRequest;
		compared = null;
		failure = null;
		opened = {};
		const wanted = allowed && peer !== '' && peer !== self;
		loading = wanted;
		if (!wanted) return;
		try {
			const [a, b] = await Promise.all([read(self), read(peer)]);
			if (request === loadRequest) compared = { a, b };
		} catch (err) {
			if (request === loadRequest) failure = err;
		} finally {
			if (request === loadRequest) loading = false;
		}
	}

	async function read(server: string): Promise<Side> {
		const [row, definition, installed] = await Promise.all([
			instances.get(server),
			manifest.export(server),
			mods.installed(server)
		]);
		return {
			name: row.name,
			build: row.game_build_id,
			launch: definition.instance,
			mods: installed.mods,
			configs: definition.configs
		};
	}

	function pick(value: string) {
		const url = new URL(page.url);
		url.searchParams.set('with', value);
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		void goto(url, { replaceState: true, keepFocus: true, noScroll: true });
	}

	function matching(count: number, one: string, many: string): string {
		return `Only differences are listed. ${count} ${count === 1 ? one : many}.`;
	}
</script>

<div class="mx-auto grid max-w-5xl gap-6 p-6">
	<header class="grid gap-1">
		<h2 class="text-2xl font-semibold tracking-tight">Compare servers</h2>
		<p class="text-sm text-muted-foreground">
			See where this server and another differ in game build, settings, mods and configuration
			files. Nothing on this page changes either server.
		</p>
	</header>

	{#if !canCompare}
		<p class="text-sm text-muted-foreground">
			Comparing needs settings, mods and configuration access on this server.
		</p>
	{:else}
		<div class="grid max-w-sm gap-2">
			<Label for="compare-with">Compare with</Label>
			<Select.Root type="single" value={other} onValueChange={pick}>
				<Select.Trigger id="compare-with">
					{candidates.find((row) => row.id === other)?.name ??
						compared?.b.name ??
						'Choose a server'}
				</Select.Trigger>
				<Select.Content>
					{#each candidates as server (server.id)}
						<Select.Item value={server.id}>{server.name}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
			<p class="text-xs text-muted-foreground">
				{!instanceList.loading && candidates.length === 0
					? 'No other server gives you settings, mods and configuration access.'
					: 'Lists the servers where you have settings, mods and configuration access.'}
			</p>
		</div>

		{#if !canCompareWith}
			<p class="text-sm text-muted-foreground">
				Comparing needs settings, mods and configuration access on the other server too.
			</p>
		{/if}

		<Problem error={failure} />

		{#if loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if compared}
			{@const { a, b } = compared}
			<p class="text-sm font-medium">{summary}</p>

			<Card.Root>
				<Card.Header>
					<Card.Title>Game and settings</Card.Title>
					<Card.Description>
						{matching(settings.length - settingDiffs.length, 'setting matches', 'settings match')}
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-4">
					{#if buildDiffers}
						<Alert.Root>
							<TriangleAlert />
							<Alert.Title>Different game builds</Alert.Title>
							<Alert.Description
								>A mod can behave differently on another game build.</Alert.Description
							>
						</Alert.Root>
					{/if}
					{#if settingDiffs.length === 0}
						<p class="text-sm text-muted-foreground">Identical: same game build and settings.</p>
					{:else}
						<div class="overflow-x-auto">
							<table class="w-full text-left text-sm">
								<thead class="text-xs text-muted-foreground">
									<tr>
										<th scope="col" class="py-2 pr-4 font-medium">Setting</th>
										<th scope="col" class="py-2 pr-4 font-medium">{a.name}</th>
										<th scope="col" class="py-2 font-medium">{b.name}</th>
									</tr>
								</thead>
								<tbody>
									{#each settingDiffs as row (row.key)}
										<tr class="border-t align-top">
											<th scope="row" class="py-2 pr-4 font-medium">{row.label}</th>
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
						{matching(modRows.length - modDiffs.length, 'mod matches', 'mods match')}
					</Card.Description>
				</Card.Header>
				<Card.Content>
					{#if modRows.length === 0}
						<p class="text-sm text-muted-foreground">Neither server has mods installed.</p>
					{:else if modDiffs.length === 0}
						<p class="text-sm text-muted-foreground">
							Identical: same mods, versions, registries and enabled state.
						</p>
					{:else}
						<div class="overflow-x-auto">
							<table class="w-full text-left text-sm">
								<thead class="text-xs text-muted-foreground">
									<tr>
										<th scope="col" class="py-2 pr-4 font-medium">Mod</th>
										<th scope="col" class="py-2 pr-4 font-medium">{a.name}</th>
										<th scope="col" class="py-2 pr-4 font-medium">{b.name}</th>
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
													{#each modChanges(row, a.name, b.name) as change (change)}
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
						{matching(configRows.length - configDiffs.length, 'file matches', 'files match')}
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-3">
					{#if configRows.length === 0}
						<p class="text-sm text-muted-foreground">Neither server has configuration files.</p>
					{:else if configDiffs.length === 0}
						<p class="text-sm text-muted-foreground">
							Identical: every configuration file matches.
						</p>
					{:else}
						{@const onlyOne = configDiffs.filter((row) => row.change !== 'changed')}
						{@const changed = configDiffs.filter((row) => row.change === 'changed')}
						{#if onlyOne.length > 0}
							<ul class="grid gap-1 text-sm">
								{#each onlyOne as row (row.key)}
									<li class="flex flex-wrap items-center gap-2">
										<span class="font-mono break-all">{row.key}</span>
										<Badge variant="outline">Only on {row.a ? a.name : b.name}</Badge>
									</li>
								{/each}
							</ul>
						{/if}
						{#if changed.length > 0}
							<p class="text-sm text-muted-foreground">
								In each file, lines marked − are from {a.name} and lines marked + are from
								{b.name}.
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
		{/if}
	{/if}
</div>

{#snippet modState(mod: ModState | null)}
	{#if mod}
		<span class="inline-flex flex-wrap items-center gap-2">
			<span class="tabular-nums">{mod.version}</span>
			<span class="text-muted-foreground">{sourceLabel[mod.source]}</span>
			{#if !mod.enabled}
				<Badge variant="secondary">disabled</Badge>
			{/if}
		</span>
	{:else}
		<span class="text-muted-foreground">Not installed</span>
	{/if}
{/snippet}
