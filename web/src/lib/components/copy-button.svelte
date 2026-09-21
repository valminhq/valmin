<script lang="ts">
	import { Button, type ButtonSize, type ButtonVariant } from '$lib/components/ui/button';
	import Copy from '@lucide/svelte/icons/copy';
	import Check from '@lucide/svelte/icons/check';

	let {
		value,
		label = 'Copy',
		variant = 'outline',
		size = 'default',
		ariaLabel = undefined
	}: {
		value: string;
		label?: string;
		variant?: ButtonVariant;
		size?: ButtonSize;
		ariaLabel?: string;
	} = $props();

	let copied = $state(false);
	let failed = $state(false);
	let clear: ReturnType<typeof setTimeout> | undefined;

	/** The clipboard API is unavailable over plain HTTP, which is how a panel on a LAN address is
	 * usually reached. A refusal there is reported rather than swallowed, because the value is
	 * something the operator has to carry somewhere else and a button that silently did nothing
	 * is indistinguishable from one that worked. */
	async function copy() {
		try {
			await navigator.clipboard.writeText(value);
		} catch {
			failed = true;
			return;
		}
		failed = false;
		copied = true;
		clearTimeout(clear);
		clear = setTimeout(() => (copied = false), 2000);
	}
</script>

<span class="inline-grid justify-items-start gap-1">
	<Button {variant} {size} onclick={copy} aria-label={ariaLabel}>
		{#if copied}<Check />{:else}<Copy />{/if}
		<span>{copied ? 'Copied' : label}</span>
	</Button>
	<span aria-live="polite" class="sr-only">{copied ? 'Copied' : ''}</span>
	{#if failed}
		<span class="text-xs text-destructive">
			This browser will not copy over an insecure connection. Select the text and copy it by hand.
		</span>
	{/if}
</span>
