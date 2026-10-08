<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions } from '$lib/api/instances';
	import { configs, type ConfigFile } from '$lib/api/configs';
	import { filterConfigs } from '$lib/config-files';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	const id = $derived(page.params.id ?? '');

	// The search text: seeded from the address's q, overridden by typing until the address changes.
	let query = $derived(page.url.searchParams.get('q') ?? '');

	let files = $state<ConfigFile[]>([]);
	const shown = $derived(filterConfigs(files, query));
	let note = $state('');
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let deleting = $state<ConfigFile | null>(null);
	let deleteOpen = $state(false);
	let busy = $state(false);

	const allowed = $derived(session.allowed(id));
	const canEdit = $derived(allowed.includes(actions.configEdit));

	$effect(() => {
		void load();
	});

	async function load() {
		loading = true;
		try {
			const listed = await configs.list(id);
			files = listed.items;
			note = listed.note ?? '';
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function askDelete(file: ConfigFile) {
		deleting = file;
		deleteOpen = true;
	}

	async function remove() {
		const target = deleting;
		deleteOpen = false;
		if (!target) return;
		busy = true;
		try {
			await configs.remove(id, target.file, target.installed_mods.length > 0);
			files = files.filter((f) => f.file !== target.file);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}
</script>

<div class="mx-auto grid max-w-3xl gap-6 p-6">
	<header class="grid gap-3">
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h2 class="text-2xl font-semibold tracking-tight">Mod configuration</h2>
				<p class="text-sm text-muted-foreground">
					{canEdit
						? 'Configure the mods installed on this server. Stop the server before editing.'
						: 'Configuration files for the mods installed on this server.'}
				</p>
			</div>
		</div>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if failure}
		<Button variant="outline" class="justify-self-start" onclick={load}
			>Retry loading mod configuration</Button
		>
	{:else if files.length === 0}
		<!-- The daemon's sentence, rendered as sent. Why a mod has no file yet is Valheim
		     knowledge, and the SPA holds none of it (F2, ADR-110). -->
		<div class="grid gap-1 rounded-lg border border-dashed p-6 text-center">
			<p class="font-medium">Nothing to configure yet</p>
			<p class="text-sm text-muted-foreground">{note}</p>
		</div>
	{:else}
		<div class="grid gap-2">
			<Label for="config-search">Search by filename or plugin</Label>
			<Input id="config-search" type="search" bind:value={query} />
			<p class="text-sm text-muted-foreground" role="status">
				{shown.length} of {files.length}
				{files.length === 1 ? 'file' : 'files'}
			</p>
		</div>
		{#if shown.length === 0}
			<div class="grid gap-1 rounded-lg border border-dashed p-6 text-center">
				<p class="font-medium">No files match</p>
				<p class="text-sm text-muted-foreground">
					No filename or plugin name contains “{query.trim()}”.
				</p>
			</div>
		{:else}
			<ul class="divide-y rounded-lg border">
				{#each shown as file (file.file)}
					<li class="flex items-center">
						<a
							class="flex min-w-0 flex-1 items-center gap-3 p-4 hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-none"
							href={resolve('/instances/[id]/configs/[file]', { id, file: file.file })}
						>
							<div class="grid min-w-0 flex-1 gap-0.5">
								<span class="truncate font-mono text-sm font-medium">{file.file}</span>
								<span class="truncate text-sm text-muted-foreground">
									{file.plugin || 'No plugin named in this file'}
								</span>
							</div>
							{#if file.dir === ''}
								<Badge variant="outline" class="shrink-0">Server root</Badge>
							{/if}
							{#if file.installed_mods.length === 0}
								<Badge variant="outline" class="shrink-0">No installed mod</Badge>
							{/if}
							<ChevronRight class="size-4 shrink-0 text-muted-foreground" />
						</a>
						{#if canEdit}
							<Button
								variant="ghost"
								size="sm"
								class="mr-2 shrink-0"
								disabled={busy}
								onclick={() => askDelete(file)}
							>
								<Trash2 />
								<span class="sr-only">Delete {file.file}</span>
							</Button>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
		{#if note}
			<p class="text-sm text-muted-foreground">{note}</p>
		{/if}
	{/if}
</div>

<Dialog.Root bind:open={deleteOpen}>
	<Dialog.Content>
		{#if deleting}
			{@const used = deleting.installed_mods}
			<Dialog.Header>
				<Dialog.Title>Delete this config file?</Dialog.Title>
				<Dialog.Description>
					{#if used.length > 0}
						{used.join(', ')}
						{used.length === 1 ? 'is' : 'are'} installed and {used.length === 1 ? 'uses' : 'use'}
						this file. Its settings go back to their defaults on the next start.
					{:else}
						No installed mod uses this file. It is deleted with the copies the panel kept of it.
					{/if}
				</Dialog.Description>
			</Dialog.Header>
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (deleteOpen = false)}>Cancel</Button>
				<Button variant="destructive" onclick={remove}>Delete config file</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
