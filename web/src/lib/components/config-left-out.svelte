<script lang="ts">
	import { manifest } from '#lib/api/manifest.js';

	/**
	 * The changed config files template files and codes leave out, with the daemon's reason for
	 * each. Renders nothing when there are none or the list cannot be read.
	 */
	let { id }: { id: string } = $props();

	let files = $state<{ file: string; reason: string }[]>([]);

	$effect(() => {
		const target = id;
		files = [];
		manifest
			.files(target)
			.then((list) => {
				if (target === id) files = list.left_out;
			})
			.catch(() => {});
	});
</script>

{#if files.length > 0}
	<details class="text-sm">
		<summary class="cursor-pointer text-muted-foreground">
			{files.length}
			{files.length === 1 ? 'changed config file is' : 'changed config files are'} not included
		</summary>
		<ul class="mt-2 grid max-h-64 gap-1 overflow-y-auto text-xs">
			{#each files as file (file.file)}
				<li><span class="font-mono">{file.file}</span> · {file.reason}</li>
			{/each}
		</ul>
	</details>
{/if}
