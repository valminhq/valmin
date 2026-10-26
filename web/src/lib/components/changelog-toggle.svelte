<script lang="ts">
	import { ApiError } from '$lib/api/errors';
	import { mods, type Changelog, type ModSource } from '$lib/api/mods';

	let {
		instanceId,
		fullName,
		version,
		source
	}: { instanceId: string; fullName: string; version: string; source?: ModSource } = $props();

	let changelog = $state<Changelog | null>(null);
	let status = $state<'idle' | 'loading' | 'none' | 'failed'>('idle');

	/** Reads the changelog of the version the first time it is opened. */
	async function load() {
		if (changelog !== null || status === 'loading' || status === 'none') return;
		status = 'loading';
		try {
			changelog = await mods.changelog(instanceId, fullName, version, source);
			status = 'idle';
		} catch (err) {
			status = err instanceof ApiError && err.status === 404 ? 'none' : 'failed';
		}
	}
</script>

<details class="text-sm" ontoggle={(e) => e.currentTarget.open && void load()}>
	<summary class="cursor-pointer text-muted-foreground">Changelog</summary>
	<div class="mt-2 grid gap-2">
		{#if status === 'loading'}
			<p class="text-muted-foreground">Reading the changelog…</p>
		{:else if status === 'none'}
			<p class="text-muted-foreground">Version {version} ships no changelog.</p>
		{:else if status === 'failed'}
			<p class="text-destructive">The changelog could not be read from the registry.</p>
		{:else if changelog}
			<pre
				class="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap">{changelog.text}</pre>
			{#if changelog.truncated}
				<p class="text-xs text-muted-foreground">The changelog is longer than shown here.</p>
			{/if}
			{#if changelog.registry_url}
				<!-- An address on the registry's own site, not a route of this app. -->
				<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
				<a class="underline" href={changelog.registry_url} target="_blank" rel="noreferrer">
					Open the package page
				</a>
			{/if}
		{/if}
	</div>
</details>
