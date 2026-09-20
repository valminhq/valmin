<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import ServerNav from '$lib/components/server-nav.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';

	let { children } = $props();

	const id = $derived(page.params.id ?? '');
	// A deep link lands here without the dashboard ever having run, so the list is loaded on
	// demand. Only the name and state are read from it; each section fetches its own row.
	$effect(() => {
		if (session.user) void instanceList.ensure();
	});
	const instance = $derived(instanceList.items.find((row) => row.id === id) ?? null);
</script>

<!--
	Which server is being changed is settled once, above every section, rather than restated
	differently by each of them. The sections are h2 below this.
-->
{#if session.user}
	<div class="border-b bg-card">
		<div class="mx-auto grid max-w-5xl gap-3 px-6 pt-4">
			<nav aria-label="Breadcrumb">
				<ol class="flex items-center gap-1 text-sm text-muted-foreground">
					<li><a class="hover:text-foreground hover:underline" href={resolve('/')}>Servers</a></li>
					<li aria-hidden="true"><ChevronRight class="size-3.5" /></li>
					<li class="min-w-0 truncate text-foreground">{instance?.name ?? 'Server'}</li>
				</ol>
			</nav>
			<div class="flex flex-wrap items-center gap-3">
				<h1 class="text-2xl font-semibold tracking-tight">{instance?.name ?? 'Server'}</h1>
				{#if instance}
					<StateBadge state={instance.state} restartRequired={instance.restart_required} />
				{/if}
			</div>
		</div>
		<ServerNav {id} />
	</div>
{/if}

<main>
	{@render children()}
</main>
