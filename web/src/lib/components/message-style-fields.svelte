<script lang="ts">
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import { LAYOUTS, type MessageStyle } from '#lib/message.js';

	/** A message's Discord layout, link, time and colour. `accent` is the colour a new custom colour
	 * starts from. */
	let {
		id,
		style = $bindable(),
		accent = '#5865F2'
	}: {
		id: string;
		style: MessageStyle;
		accent?: string;
	} = $props();

	const set = (change: Partial<MessageStyle>) => (style = { ...style, ...change });
</script>

<div class="grid gap-3">
	<div class="flex flex-wrap gap-2" role="radiogroup" aria-label="Layout">
		{#each LAYOUTS as l (l.value)}
			<label
				class="flex cursor-pointer items-center rounded-md border px-3 py-1.5 text-sm has-[:checked]:border-primary has-[:checked]:bg-primary/5 has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring"
			>
				<input
					type="radio"
					class="sr-only"
					name="{id}-layout"
					checked={style.layout === l.value}
					onchange={() => set({ layout: l.value })}
				/>
				{l.label}
			</label>
		{/each}
	</div>
	{#if style.layout !== 'plain'}
		<div class="flex flex-wrap items-center gap-x-6 gap-y-3">
			{#if style.layout === ''}
				<div class="flex items-center gap-2">
					<Switch
						id="{id}-link"
						checked={!style.hideLink}
						onCheckedChange={(on) => set({ hideLink: !on })}
					/>
					<Label for="{id}-link">Link to the server</Label>
				</div>
			{/if}
			<div class="flex items-center gap-2">
				<Switch
					id="{id}-time"
					checked={!style.hideTime}
					onCheckedChange={(on) => set({ hideTime: !on })}
				/>
				<Label for="{id}-time">Show the time</Label>
			</div>
			<div class="flex items-center gap-2">
				<Switch
					id="{id}-color"
					checked={style.color !== ''}
					onCheckedChange={(on) => set({ color: on ? accent : '' })}
				/>
				<Label for="{id}-color">Own colour</Label>
				{#if style.color}
					<input
						type="color"
						class="h-7 w-10 cursor-pointer rounded border bg-transparent"
						aria-label="Colour"
						value={style.color}
						oninput={(e) => set({ color: e.currentTarget.value })}
					/>
				{/if}
			</div>
		</div>
	{/if}
</div>
