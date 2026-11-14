<script lang="ts">
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { actions, instances, type Instance, type WatchdogMode } from '#lib/api/instances.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import Problem from '#lib/components/problem.svelte';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	/** The chosen mode: the daemon's row until edited, and the row again once the parent re-reads it. */
	let mode = $derived<WatchdogMode>(instance.watchdog);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	const canEdit = $derived(session.allowed(instance.id).includes(actions.settings));
	const canRestart = $derived(session.allowed(instance.id).includes(actions.restart));

	const changed = $derived(mode !== instance.watchdog);
	unsaved(() => changed);

	/** Saves the chosen watchdog mode. */
	async function save() {
		saving = true;
		failure = null;
		try {
			await instances.patch(instance.id, { watchdog: mode });
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
		<Card.Title>Watchdog</Card.Title>
		<Card.Description>
			Notice a server that is running but stuck, such as one hung by a mod. The game writes a status
			line every 10 minutes while it works.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<Problem error={failure} />
		<div class="grid gap-2">
			<Label for="watchdog-mode">When the server stops responding</Label>
			<NativeSelect id="watchdog-mode" disabled={!canEdit} bind:value={mode}>
				<option value="off">Do nothing</option>
				<option value="alert">Alert me</option>
				<option value="restart" disabled={!canRestart}>Alert me and restart the server</option>
			</NativeSelect>
			<p class="text-sm text-muted-foreground">
				A server counts as not responding when it has logged no status line for 15 minutes, or for
				25 minutes after it starts. A restart puts the world back to its last autosave, up to 30
				minutes old, and the watchdog restarts a server at most once an hour.
			</p>
		</div>
		{#if canEdit}
			<Button size="sm" class="justify-self-start" disabled={!changed || saving} onclick={save}>
				{saving ? 'Saving…' : 'Save watchdog'}
			</Button>
		{/if}
	</Card.Content>
</Card.Root>
