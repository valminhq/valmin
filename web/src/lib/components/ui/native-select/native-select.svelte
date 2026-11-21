<script lang="ts">
	import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
	import { cn, type WithElementRef } from "#lib/utils.js";
	import type { HTMLSelectAttributes } from "svelte/elements";

	// A native select drawn like the styled Select trigger. `class` sizes the wrapper, which the
	// select fills, so phones keep their own picker.
	let {
		ref = $bindable(null),
		value = $bindable(),
		class: className,
		"data-slot": dataSlot = "native-select",
		children,
		...restProps
	}: WithElementRef<HTMLSelectAttributes> = $props();
</script>

<span class={cn("inline-grid min-w-0", className)}>
	<select
		bind:this={ref}
		data-slot={dataSlot}
		class="peer col-start-1 row-start-1 h-8 w-full min-w-0 appearance-none rounded-lg border border-input bg-transparent py-1 pr-8 pl-2.5 text-base transition-colors outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 md:text-sm dark:bg-input/30 dark:hover:bg-input/50 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40 [&_option]:bg-popover [&_option]:text-popover-foreground [&_optgroup]:bg-popover [&_optgroup]:text-popover-foreground"
		bind:value
		{...restProps}
	>
		{@render children?.()}
	</select>
	<ChevronsUpDown
		class="pointer-events-none col-start-1 row-start-1 mr-2 size-4 self-center justify-self-end text-muted-foreground peer-disabled:opacity-50"
	/>
</span>
