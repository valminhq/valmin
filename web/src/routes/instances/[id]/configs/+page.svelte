<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { configs, type ConfigFile } from '$lib/api/configs';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import Problem from '$lib/components/problem.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let files = $state<ConfigFile[]>([]);
	let note = $state('');
	let loading = $state(true);
	let failure = $state<unknown>(null);

	const allowed = $derived(session.allowed(id));
	const canEdit = $derived(allowed.includes(actions.configEdit));

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			instance = await instances.get(id);
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
</script>

<div class="mx-auto grid max-w-3xl gap-6 p-6">
	<header class="grid gap-3">
		<Button
			variant="ghost"
			size="sm"
			class="justify-self-start"
			href={resolve('/instances/[id]', { id })}
		>
			<ArrowLeft />
			{instance?.name ?? 'Server'}
		</Button>
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h1 class="text-lg font-semibold">Settings files</h1>
				<p class="text-sm text-muted-foreground">
					{canEdit
						? 'What each mod on this server can be told to do. Edit them with the server stopped.'
						: 'What each mod on this server can be told to do.'}
				</p>
			</div>
			{#if instance}
				<StateBadge state={instance.state} restartRequired={instance.restart_required} />
			{/if}
		</div>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if files.length === 0}
		<!-- The daemon's sentence, rendered as sent. Why a mod has no file yet is Valheim
		     knowledge, and the SPA holds none of it (F2, ADR-110). -->
		<div class="grid gap-1 rounded-lg border border-dashed p-6 text-center">
			<p class="font-medium">Nothing to configure yet</p>
			<p class="text-sm text-muted-foreground">{note}</p>
		</div>
	{:else}
		<ul class="divide-y rounded-lg border">
			{#each files as file (file.file)}
				<li>
					<a
						class="flex items-center gap-3 p-4 hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-none"
						href={resolve('/instances/[id]/configs/[file]', { id, file: file.file })}
					>
						<div class="grid min-w-0 flex-1 gap-0.5">
							<span class="truncate font-mono text-sm font-medium">{file.file}</span>
							<span class="truncate text-sm text-muted-foreground">
								{file.plugin || 'No plugin named in this file'}
							</span>
						</div>
						<ChevronRight class="size-4 shrink-0 text-muted-foreground" />
					</a>
				</li>
			{/each}
		</ul>
		{#if note}
			<p class="text-sm text-muted-foreground">{note}</p>
		{/if}
	{/if}
</div>
