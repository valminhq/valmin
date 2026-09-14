<script lang="ts">
	import { page } from '$app/state';
	import { session } from '$lib/state/session.svelte';
	import ServerNav from '$lib/components/server-nav.svelte';

	let { children } = $props();
</script>

{#if session.user}
	<ServerNav id={page.params.id ?? ''} />
{/if}
{@render children()}
