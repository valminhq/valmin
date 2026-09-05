<script lang="ts">
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import * as Select from '$lib/components/ui/select';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
	import History from '@lucide/svelte/icons/history';
	import { widgets, type ConfigSetting, type ConfigValue } from '$lib/api/configs';

	/**
	 * One setting, rendered as the control the daemon chose.
	 *
	 * The branch below is on `widget` alone. Nothing here reads `type`, a range's units or a
	 * value's shape to decide what to draw — that decision is `03 §9`'s table and it lives on
	 * the server (F2, `02 §2.1`). A widget name this component does not know falls through to
	 * text, so a newer daemon can add one without a setting disappearing from the form.
	 */
	let {
		setting,
		field,
		value = $bindable(),
		asFound,
		changed = false,
		disabled = false,
		problem = ''
	}: {
		setting: ConfigSetting;
		/** `Section.Key`, and the label's target. A key is unique within its section and not
		 * across the file — `BepInEx.cfg` writes `Enabled` in two — so the key alone would
		 * give two controls one id and point a label at the wrong one. */
		field: string;
		value: ConfigValue;
		/** What this setting held before the panel first wrote the file, when that differs
		 * from what it holds now. Undefined for a file the panel has never written, and for a
		 * setting the plugin has added since. */
		asFound?: ConfigValue;
		changed?: boolean;
		disabled?: boolean;
		problem?: string;
	} = $props();

	const id = $derived(`setting-${field}`);
	const number = $derived(typeof value === 'number' ? value : Number(value));
	const range = $derived(setting.range);
	const step = $derived(setting.step > 0 ? setting.step : 'any');

	/** A default is only known when the file declared metadata for the setting. Without it
	 * the daemon sends an empty string, which is not the same claim as "the default is
	 * empty" — so the restore control stays hidden rather than writing a blank. */
	const hasDefault = $derived(setting.type !== '');
	const atDefault = $derived(String(value) === String(setting.default));

	/** Compared against the file, not against the pending edit: this marks a setting the
	 * panel has changed at some point, which is what an operator scanning a long file is
	 * looking for. A row the edit merely returned to its original value still shows it. */
	const moved = $derived(asFound !== undefined && String(asFound) !== String(setting.current));

	/** A multi-value setting is one string holding several options (`03 §9`). It is split
	 * for the checkboxes and rejoined in the daemon's own option order, so ticking the same
	 * boxes twice produces the same line. */
	const chosen = $derived(
		String(value)
			.split(',')
			.map((part) => part.trim())
			.filter(Boolean)
	);

	function toggleOption(option: string, on: boolean) {
		value = (setting.options ?? [])
			.filter((o) => (o === option ? on : chosen.includes(o)))
			.join(', ');
	}

	function takeNumber(event: Event) {
		const entered = (event.currentTarget as HTMLInputElement).valueAsNumber;
		if (!Number.isNaN(entered)) value = entered;
	}
</script>

<div
	class="grid gap-2 border-l-2 py-4 pl-4 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-start sm:gap-6 {changed
		? 'border-l-primary'
		: 'border-l-transparent'}"
>
	<div class="grid gap-1">
		<Label for={id} class="font-mono text-sm">{setting.key}</Label>
		{#if setting.description}
			<p class="max-w-prose text-sm whitespace-pre-line text-muted-foreground">
				{setting.description}
			</p>
		{/if}
		{#if problem}
			<p class="text-sm text-destructive">{problem}</p>
		{/if}
	</div>

	<div class="grid justify-items-start gap-1 sm:w-64 sm:justify-items-end">
		{#if setting.widget === widgets.toggle}
			<Switch {id} checked={value === true} onCheckedChange={(on) => (value = on)} {disabled} />
		{:else if setting.widget === widgets.slider && range}
			<div class="flex w-full items-center gap-3">
				<input
					{id}
					type="range"
					class="h-1.5 w-full min-w-0 accent-primary disabled:opacity-50"
					min={range.min}
					max={range.max}
					{step}
					value={number}
					oninput={takeNumber}
					{disabled}
				/>
				<span class="w-16 shrink-0 text-right text-sm tabular-nums">{number}</span>
			</div>
			<span class="text-xs text-muted-foreground tabular-nums">
				{range.min} to {range.max}
			</span>
		{:else if setting.widget === widgets.number}
			<Input
				{id}
				type="number"
				class="sm:w-40"
				{step}
				value={number}
				oninput={takeNumber}
				{disabled}
			/>
		{:else if setting.widget === widgets.select && setting.options}
			<Select.Root type="single" value={String(value)} onValueChange={(v) => (value = v)}>
				<Select.Trigger {id} class="sm:w-64" {disabled}>{String(value)}</Select.Trigger>
				<Select.Content>
					{#each setting.options as option (option)}
						<Select.Item value={option}>{option}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		{:else if setting.widget === widgets.multiSelect && setting.options}
			<div class="grid gap-1.5 sm:justify-items-start">
				{#each setting.options as option (option)}
					<Label class="gap-2 font-normal">
						<input
							type="checkbox"
							class="size-4 accent-primary"
							checked={chosen.includes(option)}
							onchange={(e) => toggleOption(option, e.currentTarget.checked)}
							{disabled}
						/>
						{option}
					</Label>
				{/each}
			</div>
		{:else}
			<Input {id} class="font-mono sm:w-64" bind:value={value as string} {disabled} />
		{/if}

		{#if hasDefault && !atDefault && !disabled}
			<button
				type="button"
				class="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
				onclick={() => (value = setting.default)}
			>
				<RotateCcw class="size-3" />
				Back to {String(setting.default) || 'empty'}
			</button>
		{/if}

		<!-- Offered the same way the default is, because a value worth showing is a value the
		     operator will want to put back, and one of the two being a button is the odd one. -->
		{#if moved && !disabled}
			<button
				type="button"
				class="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
				onclick={() => (value = asFound as ConfigValue)}
			>
				<History class="size-3" />
				Originally {String(asFound) || 'empty'}
			</button>
		{:else if moved}
			<span class="inline-flex items-center gap-1 text-xs text-muted-foreground">
				<History class="size-3" />
				Originally {String(asFound) || 'empty'}
			</span>
		{/if}
	</div>
</div>
