<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { base, resolve } from '$app/paths';
	import { canCompare, switchServerPath } from '$lib/nav';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import ServerNav from '$lib/components/server-nav.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import GitCompare from '@lucide/svelte/icons/git-compare';

	let { children } = $props();

	const id = $derived(page.params.id ?? '');
	// A deep link lands here without the dashboard ever having run, so the list is loaded on
	// demand. Only the name and state are read from it; each section fetches its own row.
	$effect(() => {
		if (session.user) void instanceList.ensure();
	});
	const instance = $derived(instanceList.items.find((row) => row.id === id) ?? null);
	const compareHref = $derived(resolve('/instances/[id]/compare', { id }));
	const offerCompare = $derived(
		canCompare(session.allowed(id)) && page.url.pathname !== compareHref
	);

	// Keeps the section being viewed when the chosen server offers it to this caller.
	function switchTo(event: Event & { currentTarget: HTMLSelectElement }) {
		const target = event.currentTarget.value;
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		void goto(base + switchServerPath(page.url.pathname, id, target, session.allowed(target)));
	}
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
				<div class="flex flex-wrap items-center gap-x-4 gap-y-2 sm:ml-auto">
					{#if instance && instanceList.items.length > 1}
						<label class="sr-only" for="server-switcher">Switch server</label>
						<select
							id="server-switcher"
							class="h-8 max-w-48 rounded-lg border border-input bg-background px-2.5 text-sm focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none sm:max-w-64"
							value={id}
							onchange={switchTo}
						>
							{#each instanceList.items as row (row.id)}
								<option value={row.id}>{row.name}</option>
							{/each}
						</select>
					{/if}
					{#if offerCompare}
						<a
							class="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground hover:underline"
							href={compareHref}
						>
							<GitCompare class="size-4" aria-hidden="true" /> Compare with another server
						</a>
					{/if}
				</div>
			</div>
		</div>
		<ServerNav {id} />
	</div>
{/if}

<main>
	{@render children()}
</main>
