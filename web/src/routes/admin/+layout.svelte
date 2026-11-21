<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { mayOpenAdminPage } from '#lib/admin-pages.js';
	import { session } from '#lib/state/session.svelte.js';
	import { pageNarrow } from '#lib/layout.js';

	let { children } = $props();

	// Decided once the permissions have arrived, and kept if a later re-read fails, so a form
	// being edited is not unmounted by a passing network error.
	let allowed = $state<boolean | null>(null);
	$effect(() => {
		const granted = session.permissions?.allowed_actions;
		if (granted) allowed = mayOpenAdminPage(page.url.pathname, granted);
	});
</script>

<!--
	An admin page someone cannot use answers like one that is not there, as the daemon's own
	routes do, rather than rendering its forms around refused reads.
-->
{#if allowed === null}
	<!-- The permissions are still being read. -->
{:else if allowed}
	{@render children()}
{:else}
	<main class={pageNarrow}>
		<h1 class="text-2xl font-semibold tracking-tight">Page not found</h1>
		<p class="mt-2 text-sm text-muted-foreground">
			This page does not exist, or you do not have access to it.
			<a class="underline" href={resolve('/')}>Back to the server list</a>
		</p>
	</main>
{/if}
