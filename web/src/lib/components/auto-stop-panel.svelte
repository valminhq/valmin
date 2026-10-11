<script lang="ts">
	import { unsaved } from '$lib/state/dirty.svelte';
	import {
		AUTO_STOP_MAX,
		AUTO_STOP_MIN,
		actions,
		instances,
		type Instance
	} from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	const DEFAULT_MINUTES = 30;

	let enabled = $state(false);
	let minutes = $state<number | null>(DEFAULT_MINUTES);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	const canEdit = $derived(
		session.allowed(instance.id).includes(actions.settings) &&
			session.allowed(instance.id).includes(actions.stop)
	);

	/** Takes the daemon's row as the form's baseline, and again after the parent re-reads it. */
	$effect(() => {
		enabled = instance.auto_stop_minutes > 0;
		minutes = instance.auto_stop_minutes > 0 ? instance.auto_stop_minutes : DEFAULT_MINUTES;
	});

	const wanted = $derived(enabled ? minutes : 0);
	const valid = $derived(
		!enabled ||
			(minutes !== null &&
				Number.isInteger(minutes) &&
				minutes >= AUTO_STOP_MIN &&
				minutes <= AUTO_STOP_MAX)
	);
	const changed = $derived(wanted !== instance.auto_stop_minutes);
	unsaved(() => changed);

	/** Saves the delay, or 0 when the switch is off. */
	async function save() {
		if (!valid || wanted === null) return;
		saving = true;
		failure = null;
		try {
			await instances.patch(instance.id, { auto_stop_minutes: wanted });
			onchange?.();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Auto-stop</Card.Title>
		<Card.Description>
			Stop this server after a while with no players, to save power. Players cannot join until it is
			started again.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<Problem error={failure} />
		<div class="flex items-center justify-between gap-4">
			<div class="grid gap-1">
				<Label for="auto-stop">Stop when empty</Label>
				<p class="text-sm text-muted-foreground">
					While the player count is unknown, which can last up to 10 minutes after the server
					starts, the server counts as occupied.
				</p>
			</div>
			<Switch id="auto-stop" disabled={!canEdit} bind:checked={enabled} />
		</div>
		{#if enabled}
			<div class="grid gap-2">
				<Label for="auto-stop-minutes">Minutes with no players</Label>
				<Input
					id="auto-stop-minutes"
					type="number"
					min={AUTO_STOP_MIN}
					max={AUTO_STOP_MAX}
					step="1"
					disabled={!canEdit}
					aria-invalid={!valid}
					aria-describedby="auto-stop-minutes-error"
					bind:value={minutes}
				/>
				{#if !valid}
					<p id="auto-stop-minutes-error" class="text-xs text-destructive">
						Enter a whole number from {AUTO_STOP_MIN} to {AUTO_STOP_MAX}.
					</p>
				{/if}
			</div>
		{/if}
		{#if canEdit}
			<Button
				size="sm"
				class="justify-self-start"
				disabled={!valid || !changed || saving}
				onclick={save}
			>
				{saving ? 'Saving…' : 'Save auto-stop'}
			</Button>
		{/if}
	</Card.Content>
</Card.Root>
