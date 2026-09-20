<script module lang="ts">
	/**
	 * Moves focus to the first control a rejected submit marked invalid, so the correction is
	 * where the caret is rather than somewhere the operator has to go looking. Called after the
	 * errors have been rendered.
	 */
	export function focusFirstInvalid(root: ParentNode = document): void {
		const first = root.querySelector<HTMLElement>('[aria-invalid="true"]');
		first?.focus();
		first?.scrollIntoView({ block: 'center', behavior: 'smooth' });
	}
</script>

<script lang="ts">
	import type { Snippet } from 'svelte';
	import { Label } from '$lib/components/ui/label';

	/**
	 * A labelled form field with its hint and its error wired to the control. The control is
	 * rendered by the caller and receives the `aria-invalid` and `aria-describedby` pair to
	 * spread onto itself, so the association cannot drift from the markup that needs it.
	 */
	let {
		id,
		label,
		hint,
		error,
		required = false,
		children
	}: {
		id: string;
		label: string;
		hint?: string;
		error?: string;
		required?: boolean;
		children: Snippet<[Record<string, string | undefined>]>;
	} = $props();

	// Built from what is actually rendered: an aria-describedby naming an element that is not
	// there is read as nothing at all.
	const described = $derived(
		[hint ? `${id}-hint` : '', error ? `${id}-error` : ''].filter(Boolean).join(' ')
	);
	const attributes = $derived({
		'aria-invalid': error ? 'true' : undefined,
		'aria-describedby': described || undefined
	});
</script>

<div class="grid gap-2">
	<Label for={id}>
		{label}
		{#if required}
			<span class="text-muted-foreground" aria-hidden="true">*</span>
			<span class="sr-only">(required)</span>
		{/if}
	</Label>
	{@render children(attributes)}
	{#if hint}
		<p id={`${id}-hint`} class="text-sm text-muted-foreground">{hint}</p>
	{/if}
	{#if error}
		<p id={`${id}-error`} class="text-sm text-destructive" role="alert">{error}</p>
	{/if}
</div>
