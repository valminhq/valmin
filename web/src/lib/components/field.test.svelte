<script lang="ts">
	import Field from './field.svelte';

	// A field needs a control to spread its attributes onto, which a props-only render cannot
	// supply. This is that control.
	let {
		id,
		label,
		hint,
		error,
		required = false
	}: {
		id: string;
		label: string;
		hint?: string;
		error?: string;
		required?: boolean;
	} = $props();
</script>

<Field {id} {label} {hint} {error} {required}>
	{#snippet children(field)}
		<input {id} {...field} />
	{/snippet}
</Field>
