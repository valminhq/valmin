<script lang="ts">
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';

	/** A rule's quiet window: an on switch, a start and end clock time, and a timezone picked from
	 * the datalist `zonesList` names. */
	let {
		id,
		zonesList,
		on = $bindable(),
		start = $bindable(),
		end = $bindable(),
		zone = $bindable(),
		invalid = false,
		hint
	}: {
		id: string;
		zonesList: string;
		on: boolean;
		start: string;
		end: string;
		zone: string;
		invalid?: boolean;
		hint: string;
	} = $props();
</script>

<div class="flex items-center gap-2">
	<Switch {id} bind:checked={on} />
	<Label for={id}>Quiet hours</Label>
</div>
{#if on}
	<div class="grid gap-3 sm:grid-cols-[8rem_8rem_1fr]">
		<div class="grid gap-2">
			<Label for="{id}-start">From</Label>
			<Input id="{id}-start" type="time" step="60" bind:value={start} />
		</div>
		<div class="grid gap-2">
			<Label for="{id}-end">Until</Label>
			<Input id="{id}-end" type="time" step="60" bind:value={end} />
		</div>
		<div class="grid gap-2">
			<Label for="{id}-zone">Timezone</Label>
			<Input id="{id}-zone" list={zonesList} aria-invalid={zone.trim() === ''} bind:value={zone} />
		</div>
	</div>
	{#if invalid}
		<p class="text-xs text-destructive">
			Set a start, an end and a timezone; start and end must differ.
		</p>
	{/if}
	<p class="text-sm text-muted-foreground">{hint}</p>
{/if}
