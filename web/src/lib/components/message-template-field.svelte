<script lang="ts">
	import { Button } from '#lib/components/ui/button/index.js';
	import { MAX_TEMPLATE } from '#lib/message.js';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';

	/** A message template: the text, a way back to the default, and buttons that insert a
	 * placeholder where the cursor is. */
	let {
		id,
		value = $bindable(),
		fallback,
		placeholders
	}: {
		id: string;
		value: string;
		/** The default message. */
		fallback: string;
		placeholders: string[];
	} = $props();

	let box = $state<HTMLTextAreaElement>();

	function insert(name: string) {
		const at = box?.selectionStart ?? value.length;
		const end = box?.selectionEnd ?? value.length;
		const token = `{${name}}`;
		value = value.slice(0, at) + token + value.slice(end);
		requestAnimationFrame(() => {
			box?.focus();
			box?.setSelectionRange(at + token.length, at + token.length);
		});
	}
</script>

<div class="grid gap-2">
	<div class="flex items-center justify-between gap-2">
		<label for={id} class="text-sm font-medium">Message</label>
		{#if value.trim() !== fallback && value.trim() !== ''}
			<Button variant="ghost" size="sm" class="h-7" onclick={() => (value = fallback)}>
				<RotateCcw /> Use the default
			</Button>
		{/if}
	</div>
	<textarea
		{id}
		bind:this={box}
		rows={Math.min(6, Math.max(2, fallback.split('\n').length))}
		class="w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 aria-invalid:border-destructive aria-invalid:ring-destructive/20"
		aria-invalid={value.length > MAX_TEMPLATE}
		bind:value></textarea>
	<div class="flex flex-wrap items-center gap-1.5">
		<span class="text-xs text-muted-foreground">Insert:</span>
		{#each placeholders as name (name)}
			<button
				type="button"
				class="rounded border bg-muted/50 px-1.5 py-0.5 font-mono text-xs hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
				onclick={() => insert(name)}
			>
				{`{${name}}`}
			</button>
		{/each}
	</div>
</div>
