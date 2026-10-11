<script lang="ts">
	import { onMount } from 'svelte';
	import { goto, replaceState } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { rcloneSetups } from '$lib/api/remote-backups';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import Problem from '$lib/components/problem.svelte';

	// The provider's redirect, read once: the address bar loses it before anything is sent.
	const query = page.url.searchParams;
	const callback = {
		state: query.get('state') ?? '',
		code: query.get('code') ?? '',
		error: query.get('error') ?? ''
	};
	const setupId = callback.state.split('.')[0];
	const returnTo = resolve('/admin/remote-backups') + '?setup=' + encodeURIComponent(setupId);

	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));
	let failure = $state<unknown>(null);
	let sent = false;

	onMount(() => replaceState(resolve('/admin/rclone-callback'), {}));

	$effect(() => {
		if (!allowed || !setupId || sent) return;
		sent = true;
		void submit();
	});

	async function submit() {
		try {
			await rcloneSetups.advance(setupId, callback);
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			await goto(returnTo, { replaceState: true });
		} catch (err) {
			failure = err;
		}
	}
</script>

<main class="mx-auto grid w-full max-w-3xl gap-6 p-4 sm:p-6">
	<h1 class="text-2xl font-semibold">Connect storage account</h1>
	{#if !setupId}
		<p>This page finishes a storage provider's sign-in. Start from Remote backups.</p>
		<Button class="justify-self-start" href={resolve('/admin/remote-backups')}
			>Remote backups</Button
		>
	{:else if session.permissions && !allowed}
		<p>You do not have permission to manage remote storage.</p>
	{:else if failure}
		<Problem error={failure} />
		<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
		<Button class="justify-self-start" href={returnTo}>Return to setup</Button>
	{:else}
		<p role="status">Finishing authorization…</p>
	{/if}
</main>
