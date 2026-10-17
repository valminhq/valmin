<script lang="ts">
	import type { GameOptions } from '$lib/api/instances';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';

	/** World modifiers as the game's own settings panel shows them: a slider per axis and a
	 * switch per world key, with typed values as the manual fallback. */
	let {
		options,
		modifiers = $bindable(),
		disabled = false
	}: { options: GameOptions; modifiers: Record<string, string>; disabled?: boolean } = $props();

	let manual = $state(false);
	const current = (key: string) => (modifiers[key] ?? '').trim();
	// A value no slider position holds can only be shown, and kept, as text.
	const unlisted = $derived(
		options.modifiers.some((m) => !m.values.some((o) => o.value === current(m.key)))
	);
	const typing = $derived(manual || unlisted);

	function set(key: string, value: string) {
		if (value === '') delete modifiers[key];
		else modifiers[key] = value;
	}
</script>

<div class="grid gap-4">
	<div class="flex items-start justify-between gap-4">
		<p class="text-sm text-muted-foreground">
			Normal uses the server default, or the preset's value.
		</p>
		<div class="flex shrink-0 items-center gap-2">
			<Label for="modifiers-manual">Manual</Label>
			<Switch
				id="modifiers-manual"
				disabled={disabled || unlisted}
				bind:checked={() => typing, (on) => (manual = on)}
			/>
		</div>
	</div>

	{#each options.modifiers as m (m.key)}
		{@const index = m.values.findIndex((o) => o.value === current(m.key))}
		<div
			class="grid grid-cols-[1fr_auto] items-center gap-x-4 gap-y-2 sm:grid-cols-[9rem_1fr_6rem]"
		>
			<Label for={`modifier-${m.key}`}>{m.label}</Label>
			{#if typing}
				<Input
					id={`modifier-${m.key}`}
					class="col-span-2"
					value={modifiers[m.key] ?? ''}
					placeholder="Normal"
					autocomplete="off"
					{disabled}
					oninput={(event) => set(m.key, event.currentTarget.value)}
				/>
			{:else}
				<span
					class={[
						'text-right text-sm sm:col-start-3 sm:row-start-1 sm:text-left',
						m.values[index].value === '' ? 'text-muted-foreground' : 'font-medium'
					]}>{m.values[index].label}</span
				>
				<input
					id={`modifier-${m.key}`}
					type="range"
					min="0"
					max={m.values.length - 1}
					step="1"
					value={index}
					aria-valuetext={m.values[index].label}
					class="col-span-2 w-full accent-primary disabled:opacity-50 sm:col-span-1 sm:col-start-2 sm:row-start-1"
					{disabled}
					oninput={(event) => set(m.key, m.values[Number(event.currentTarget.value)].value)}
				/>
			{/if}
		</div>
	{/each}

	<div class="grid gap-3 border-t pt-4 sm:grid-cols-3">
		{#each options.world_keys as key (key.value)}
			<div class="flex items-center gap-2">
				<Switch
					id={`world-key-${key.value}`}
					{disabled}
					bind:checked={() => modifiers[key.value] === 'on', (on) => set(key.value, on ? 'on' : '')}
				/>
				<Label for={`world-key-${key.value}`}>{key.label}</Label>
			</div>
		{/each}
	</div>
</div>
