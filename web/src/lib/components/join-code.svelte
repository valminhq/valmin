<script lang="ts">
	/** The crossplay join code the running server logged. It is how a player without a
	 * server browser gets in, so it is rendered whole and selectable rather than truncated. */
	let { code }: { code: string } = $props();
</script>

<span class="inline-flex w-fit items-center gap-1.5 rounded-md bg-muted px-2 py-0.5">
	<span class="text-xs text-muted-foreground">join code</span>
	<code class="font-mono text-sm font-semibold tracking-wider select-all">{code}</code>
</span>
