<script lang="ts">
	import type { HTMLAttributes } from 'svelte/elements';
	import type { MessageStyle } from '$lib/message';

	/** How one message reads in Discord, laid out as the daemon renders it (notify.discordPayload). */
	let {
		server,
		headline,
		text,
		style,
		accent,
		...rest
	}: {
		server: string;
		headline: string;
		text: string;
		style: MessageStyle;
		accent: string;
	} & Omit<HTMLAttributes<HTMLElement>, 'style'> = $props();

	const title = $derived(server ? `${server} · ${headline}` : headline);
	const body = $derived(style.layout === '' ? text : text || headline);
	const time = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

	/** The text split at Discord's bold markers, which is as much markdown as the preview shows. */
	const parts = $derived(body.split('**').map((part, i) => ({ part, bold: i % 2 === 1 })));
</script>

{#snippet message()}
	<span class="text-sm break-words whitespace-pre-wrap"
		>{#each parts as { part, bold }, i (i)}{#if bold}<strong>{part}</strong
				>{:else}{part}{/if}{/each}</span
	>
{/snippet}

<figure class="grid gap-1.5" {...rest}>
	<figcaption class="text-xs text-muted-foreground">In Discord it looks like this</figcaption>
	{#if style.layout === 'plain'}
		<div class="rounded-md bg-muted/60 px-3 py-2">{@render message()}</div>
	{:else}
		<div
			class="grid gap-1 rounded-md border-l-4 bg-muted/60 px-3 py-2"
			style:border-left-color={style.color || accent}
		>
			{#if style.layout === ''}
				<span
					class={['text-sm font-semibold', !style.hideLink && 'text-sky-700 dark:text-sky-400']}
				>
					{title}
				</span>
			{/if}
			{#if body}{@render message()}{/if}
			{#if !style.hideTime}
				<span class="text-xs text-muted-foreground">Today at {time}</span>
			{/if}
		</div>
	{/if}
</figure>
