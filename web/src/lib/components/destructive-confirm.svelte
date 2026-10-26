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
		onconfirm
	}: {
		open?: boolean;
		/** The thing being destroyed. It must be typed back, exactly. */
		name: string;
		title: string;
		description: string;
		confirmLabel?: string;
		onconfirm: () => void;
	} = $props();

	let typed = $state('');
	const matches = $derived(typed === name);

	$effect(() => {
		if (!open) typed = '';
	});
</script>

<!--
	Every destructive action names the thing being destroyed, and here the operator
	has to name it back. A dialog whose only content is "Are you sure?" is one an admin
	dismisses by reflex, and on this panel the thing on the other side of the reflex is
	somebody's world.
-->
<Dialog.Root bind:open>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>{title}</Dialog.Title>
			<Dialog.Description>{description}</Dialog.Description>
		</Dialog.Header>
		<div class="grid gap-2">
			<Label for="confirm-name"
				>Type <span class="font-mono font-semibold">{name}</span> to confirm</Label
			>
			<Input id="confirm-name" bind:value={typed} autocomplete="off" />
		</div>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (open = false)}>Cancel</Button>
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
