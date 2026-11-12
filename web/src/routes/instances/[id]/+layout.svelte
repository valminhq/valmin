<script lang="ts">
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { canCompare, switchServerPath } from '#lib/nav.js';
	import { session } from '#lib/state/session.svelte.js';
	import { instanceList } from '#lib/state/instances.svelte.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import ServerNav from '#lib/components/server-nav.svelte';
	import StateBadge from '#lib/components/state-badge.svelte';
	import ServerControls from '#lib/components/server-controls.svelte';
	import Problem from '#lib/components/problem.svelte';
	import GitCompare from '@lucide/svelte/icons/git-compare';

	let { children } = $props();

	const id = $derived(page.params.id ?? '');
	// A deep link lands here without the dashboard ever having run, so the list is loaded on
	// demand. Only the name and state are read from it; each section fetches its own row.
	$effect(() => {
		if (session.user) void instanceList.ensure();
	});
	const instance = $derived(instanceList.items.find((row) => row.id === id) ?? null);
	// The socket cannot say what changed while it was gone, so a reconnect re-reads the list.
	$effect(() => socket.onConnected(() => void instanceList.load()));

	// A refused request is about one server in one state; it goes when either changes.
	let failure = $state<unknown>(null);
	$effect(() => {
		void id;
		void instance?.state;
		failure = null;
	});

	const compareHref = $derived(resolve('/instances/[id]/compare', { id }));
	const offerCompare = $derived(
		canCompare(session.allowed(id)) && page.url.pathname !== compareHref
	);

	// Keeps the section being viewed when the chosen server offers it to this caller.
	function switchTo(event: Event & { currentTarget: HTMLSelectElement }) {
		const target = event.currentTarget.value;
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		void goto(switchServerPath(page.url.pathname, id, target, session.allowed(target)));
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
					{#key id}
						<ServerControls
							{instance}
							allowed={session.allowed(id)}
							onfailure={(error) => (failure = error)}
						/>
					{/key}
				{/if}
				<div class="flex flex-wrap items-center gap-x-4 gap-y-2 sm:ml-auto">
					{#if instance && instanceList.items.length > 1}
						<label class="sr-only" for="server-switcher">Switch server</label>
						<NativeSelect
							id="server-switcher"
							class="max-w-48 sm:max-w-64"
							value={id}
							onchange={switchTo}
						>
							{#each instanceList.items as row (row.id)}
								<option value={row.id}>{row.name}</option>
							{/each}
						</NativeSelect>
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
			<Problem error={failure} />
		</div>
		<ServerNav {id} />
	</div>
{/if}

<!-- Keyed by server, so a section never keeps one server's data while addressing another. -->
<main class="mx-auto max-w-6xl px-4 py-6 sm:px-6">
	{#key id}
		{@render children()}
	{/key}
</main>
