<script lang="ts">
	import { cn, type WithElementRef } from "#lib/utils.js";
	import type { HTMLSelectAttributes } from "svelte/elements";

	let {
		ref = $bindable(null),
		value = $bindable(),
		class: className,
		"data-slot": dataSlot = "native-select",
		children,
		...restProps
	}: WithElementRef<HTMLSelectAttributes> = $props();
</script>

<select
	bind:this={ref}
	data-slot={dataSlot}
	class={cn(
		"h-8 min-w-0 rounded-lg border border-input bg-background px-2.5 text-base transition-colors outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 md:text-sm dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40",
		className
	)}
	bind:value
	{...restProps}
>
	{@render children?.()}
</select>
