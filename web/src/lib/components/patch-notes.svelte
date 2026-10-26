<script lang="ts">
	import { instances, STEAM_NEWS_URL, type PatchNote } from '$lib/api/instances';

	let notes = $state<PatchNote[] | null>(null);
	let failed = $state(false);
	let loading = false;

	/** Asks for the notes the first time the list is opened, so a page that only mentions them
	 * does not reach Steam. */
	async function load() {
		if (notes !== null || loading) return;
		loading = true;
		failed = false;
		try {
			notes = (await instances.patchNotes()).notes;
		} catch {
			failed = true;
		} finally {
			loading = false;
		}
	}

	const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' });
</script>

<details
	class="rounded-md border px-3 py-2 text-sm"
	ontoggle={(e) => e.currentTarget.open && void load()}
>
	<summary class="cursor-pointer font-medium">Recent game patch notes</summary>
	<div class="mt-3 grid gap-3">
		{#if failed}
			<p class="text-muted-foreground">
				Steam's news could not be reached.
				<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
				<a class="underline" href={STEAM_NEWS_URL} target="_blank" rel="noreferrer">
					Read the patch notes on Steam
				</a>
			</p>
		{:else if notes === null}
			<p class="text-muted-foreground">Loading patch notes…</p>
		{:else if notes.length === 0}
			<p class="text-muted-foreground">
				Steam lists no recent patch notes.
				<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
				<a class="underline" href={STEAM_NEWS_URL} target="_blank" rel="noreferrer">
					Open the game's news on Steam
				</a>
			</p>
		{:else}
			<p class="text-xs text-muted-foreground">
				Steam does not say which patch a build id belongs to, so these are the latest patches, not
				only the ones this update adds.
			</p>
			{#each notes as note (note.url)}
				<details class="rounded-md border px-3 py-2">
					<summary class="cursor-pointer">
						<span class="font-medium">{note.title}</span>
						<span class="text-muted-foreground"> · {day(note.published_at)}</span>
					</summary>
					<pre
						class="mt-2 max-h-72 overflow-auto font-sans text-sm whitespace-pre-wrap">{note.text}</pre>
					<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
					<a class="mt-2 inline-block underline" href={note.url} target="_blank" rel="noreferrer">
						Open on Steam
					</a>
				</details>
			{/each}
		{/if}
	</div>
</details>
