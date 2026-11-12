<script lang="ts">
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';

	let {
		open = $bindable(false),
		name,
		title,
		description,
		confirmLabel = 'Delete',
		cancelLabel = 'Cancel',
		onconfirm
	}: {
		open?: boolean;
		/** The thing being destroyed, to be typed back exactly. Leave it out for an action
		 * that loses no world or server, and the dialog asks for a click alone. */
		name?: string;
		title: string;
		description: string;
		confirmLabel?: string;
		cancelLabel?: string;
		onconfirm: () => void;
	} = $props();

	let typed = $state('');
	const matches = $derived(name === undefined || typed === name);

	$effect(() => {
		if (!open) typed = '';
	});
</script>

<!--
	Confirms a destructive action. Given a name, the operator has to type it back, which
	is for anything that loses a world or a server. Without one it is a plain confirmation.
-->
<Dialog.Root bind:open>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>{title}</Dialog.Title>
			<Dialog.Description>{description}</Dialog.Description>
		</Dialog.Header>
		{#if name !== undefined}
			<div class="grid gap-2">
				<Label for="confirm-name"
					>Type <span class="font-mono font-semibold">{name}</span> to confirm</Label
				>
				<Input id="confirm-name" bind:value={typed} autocomplete="off" />
			</div>
		{/if}
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (open = false)}>{cancelLabel}</Button>
			<Button
				variant="destructive"
				disabled={!matches}
				onclick={() => {
					open = false;
					onconfirm();
				}}
			>
				{confirmLabel}
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
