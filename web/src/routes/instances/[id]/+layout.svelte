<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { base, resolve } from '$app/paths';
	import { canCompare, switchServerPath } from '$lib/nav';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import ServerNav from '$lib/components/server-nav.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
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
		<div class="mx-auto grid max-w-6xl gap-3 px-4 pt-4 sm:px-6">
			<div class="flex flex-wrap items-center gap-3">
				<h1 class="text-2xl font-semibold tracking-tight">{instance?.name ?? 'Server'}</h1>
				{#if instance}
					<StateBadge
						state={instance.state}
						restartRequired={instance.restart_required}
						pendingRestart={instance.pending_restart}
					/>
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

<main class="mx-auto max-w-6xl px-4 py-6 sm:px-6">
	{@render children()}
</main>
