<script lang="ts">
	import type { Snippet } from 'svelte';
	import { Button } from '#lib/components/ui/button/index.js';

	// The save row at the bottom of a card that edits its own settings: Save, and once something
	// differs, a note and Discard beside it. Save stays disabled until then. Without onsave, Save
	// submits the form around it.
	let {
		saveLabel,
		changed,
		busy = false,
		saving = busy,
		creating = false,
		invalid = false,
		ondiscard,
		onsave,
		children
	}: {
		saveLabel: string;
		changed: boolean;
		/** Whether this form's save is running. Disables Discard and Save. */
		busy?: boolean;
		/** Whether the Save label reads Saving…, when only this form's save should say so. */
		saving?: boolean;
		/** Whether Save is offered with nothing changed, to create what the form describes. */
		creating?: boolean;
		invalid?: boolean;
		ondiscard: () => void;
		onsave?: () => void;
		/** Further actions that belong beside Save. */
		children?: Snippet;
	} = $props();
</script>

<div class="flex flex-wrap items-center gap-2">
	<Button
		size="sm"
		type={onsave ? 'button' : 'submit'}
		disabled={!(changed || creating) || invalid || busy}
		onclick={onsave}
	>
		{saving ? 'Saving…' : saveLabel}
	</Button>
	{#if changed}
		<Button variant="ghost" size="sm" disabled={busy} onclick={ondiscard}>Discard changes</Button>
		<span class="text-sm text-muted-foreground">Unsaved changes</span>
	{/if}
	{@render children?.()}
</div>
