<script lang="ts">
	import type { HTMLInputAttributes } from 'svelte/elements';
	import { buttonVariants } from '#lib/components/ui/button/index.js';
	import { cn } from '#lib/utils.js';

	// A file input drawn as an outline button with the chosen name beside it. `folder` picks a
	// whole directory and names it rather than its files.
	let {
		id,
		files = $bindable(),
		folder = false,
		class: className,
		...restProps
	}: Omit<HTMLInputAttributes, 'type' | 'files'> & {
		id: string;
		files?: FileList;
		folder?: boolean;
	} = $props();

	const chosen = $derived.by(() => {
		if (!files || files.length === 0) return 'Nothing chosen';
		if (folder) return files[0].webkitRelativePath.split('/')[0] || `${files.length} files`;
		return files.length === 1 ? files[0].name : `${files.length} files`;
	});
</script>

<span class={cn('flex min-w-0 flex-wrap items-center gap-2', className)}>
	<input
		{id}
		type="file"
		class="peer sr-only"
		bind:files
		{...folder ? { webkitdirectory: true, directory: true } : {}}
		{...restProps}
	/>
	<label
		for={id}
		class={cn(
			buttonVariants({ variant: 'outline', size: 'sm' }),
			'cursor-pointer peer-focus-visible:border-ring peer-focus-visible:ring-3 peer-focus-visible:ring-ring/50 peer-disabled:pointer-events-none peer-disabled:opacity-50'
		)}
	>
		{folder ? 'Choose folder' : restProps.multiple ? 'Choose files' : 'Choose file'}
	</label>
	<span class="min-w-0 text-sm wrap-anywhere text-muted-foreground">{chosen}</span>
</span>
