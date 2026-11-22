<script lang="ts">
	import { instances, type Instance, type SetupBeforeUpdate } from '#lib/api/instances.js';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import Problem from '#lib/components/problem.svelte';
	import SaveFooter from '#lib/components/save-footer.svelte';

	// Whether a mod update first saves the server's setup as an automatic one, and whether that
	// setup keeps package files. The two switches are one stored setting: full, light or off.
	let { instance, onchange }: { instance: Instance; onchange?: (row: Instance) => void } = $props();

	let enabled = $state(true);
	let keepPackages = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	const stored = $derived(instance.setup_before_update);
	const choice = $derived<SetupBeforeUpdate>(!enabled ? 'off' : keepPackages ? 'full' : 'light');
	const changed = $derived(choice !== stored);
	unsaved(() => changed);

	/** Puts the switches back on the daemon's value. Re-runs only when that value changes, so a
	 * re-read of an unchanged row keeps unsaved switches. */
	function reset() {
		enabled = stored !== 'off';
		keepPackages = stored !== 'light';
	}
	$effect(reset);

	async function save() {
		saving = true;
		failure = null;
		try {
			const row = await instances.patch(instance.id, { setup_before_update: choice });
			onchange?.(row);
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Before mod updates</Card.Title>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<div class="flex items-center justify-between gap-4">
			<div class="grid gap-1">
				<Label for="setup-before-update">Save the setup before mod updates</Label>
				<p class="text-sm text-muted-foreground">
					Each mod update first saves this server's mods and configuration files as an automatic
					setup, linked to the world backup the update takes. Restore it here to undo the update.
					The newest three automatic setups are kept, with their world backups.
				</p>
			</div>
			<Switch id="setup-before-update" bind:checked={enabled} />
		</div>
		{#if enabled}
			<div class="flex items-center justify-between gap-4">
				<div class="grid gap-1">
					<Label for="setup-before-update-packages"
						>Keep package files (restore works offline)</Label
					>
					<p class="text-sm text-muted-foreground">
						{keepPackages
							? 'The setup keeps a copy of every mod package, so a restore needs neither the registry nor the download cache. Packages the download cache holds take no extra space.'
							: 'Light: the setup keeps only mods that cannot be downloaded again, and a restore fetches the rest from the download cache or the registry. It fails for a version the registry has removed.'}
					</p>
				</div>
				<Switch id="setup-before-update-packages" bind:checked={keepPackages} />
			</div>
		{/if}
		<Problem error={failure} />
		<SaveFooter saveLabel="Save" {changed} busy={saving} ondiscard={reset} onsave={save} />
	</Card.Content>
</Card.Root>
