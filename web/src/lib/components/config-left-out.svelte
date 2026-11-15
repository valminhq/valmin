<script lang="ts">
	import { configs } from '#lib/api/configs.js';

	/**
	 * The server's config files that saved setups, template files and template codes do not
	 * carry, as the daemon marks them. Renders nothing when there are none or the list cannot be
	 * read.
	 */
	let { id }: { id: string } = $props();

	let files = $state<string[]>([]);

	$effect(() => {
		const target = id;
		files = [];
		configs
			.list(target)
			.then((list) => {
				if (target === id) files = list.items.filter((f) => !f.portable).map((f) => f.file);
			})
			.catch(() => {});
	});
</script>

{#if files.length > 0}
	<details class="text-sm">
		<summary class="cursor-pointer text-muted-foreground">
			{files.length}
			{files.length === 1 ? 'config file is' : 'config files are'} not included
		</summary>
		<ul class="mt-2 grid max-h-64 gap-1 overflow-y-auto font-mono text-xs">
			{#each files as file (file)}
				<li>{file}</li>
			{/each}
		</ul>
	</details>
{/if}
