<script lang="ts">
	import {
		instances,
		SETUP_KEPT_MAX,
		SETUP_KEPT_MIN,
		type Instance,
		type PatchInstance,
		type SetupBeforeUpdate
	} from '#lib/api/instances.js';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import Problem from '#lib/components/problem.svelte';
	import SaveFooter from '#lib/components/save-footer.svelte';

	// Whether a mod change first saves the server's setup as an automatic one, whether that setup
	// keeps package files, and how many automatic setups and world backups are kept. The two
	// switches are one stored setting: full, light or off.
	let { instance, onchange }: { instance: Instance; onchange?: (row: Instance) => void } = $props();

	let enabled = $state(true);
	let keepPackages = $state(true);
	let versions = $state<number | null>(10);
	let worlds = $state<number | null>(3);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	const stored = $derived(instance.setup_before_update);
	const storedVersions = $derived(instance.setup_versions_kept);
	const storedWorlds = $derived(instance.setup_worlds_kept);
	const choice = $derived<SetupBeforeUpdate>(!enabled ? 'off' : keepPackages ? 'full' : 'light');
	const versionsValid = $derived(
		versions !== null &&
			Number.isInteger(versions) &&
			versions >= SETUP_KEPT_MIN &&
			versions <= SETUP_KEPT_MAX
	);
	const worldsValid = $derived(
		worlds !== null && Number.isInteger(worlds) && worlds >= 0 && worlds <= SETUP_KEPT_MAX
	);
	const changed = $derived(
		choice !== stored || versions !== storedVersions || worlds !== storedWorlds
	);
	unsaved(() => changed);

	/** Puts the fields back on the daemon's values. Re-runs only when those values change, so a
	 * re-read of an unchanged row keeps unsaved edits. */
	function reset() {
		enabled = stored !== 'off';
		keepPackages = stored !== 'light';
		versions = storedVersions;
		worlds = storedWorlds;
	}
	$effect(reset);

	async function save() {
		if (versions === null || worlds === null || !versionsValid || !worldsValid) return;
		const body: PatchInstance = {};
		if (choice !== stored) body.setup_before_update = choice;
		if (versions !== storedVersions) body.setup_versions_kept = versions;
		if (worlds !== storedWorlds) body.setup_worlds_kept = worlds;
		saving = true;
		failure = null;
		try {
			const row = await instances.patch(instance.id, body);
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
		<Card.Title>Before mod changes</Card.Title>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<div class="flex items-center justify-between gap-4">
			<div class="grid gap-1">
				<Label for="setup-before-update">Save the setup before mod changes</Label>
				<p class="text-sm text-muted-foreground">
					Each mod install, removal and update first saves this server's mods and configuration
					files as an automatic setup. An update links it to the world backup it takes. Restore it
					here to undo the change. Disabling or enabling a mod saves nothing.
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
		<div class="grid gap-3 sm:grid-cols-2">
			<div class="grid content-start gap-2">
				<Label for="setup-versions-kept">Automatic setups to keep</Label>
				<Input
					id="setup-versions-kept"
					type="number"
					min={SETUP_KEPT_MIN}
					max={SETUP_KEPT_MAX}
					step="1"
					aria-invalid={!versionsValid}
					aria-describedby="setup-versions-kept-error"
					bind:value={versions}
				/>
				{#if !versionsValid}
					<p id="setup-versions-kept-error" class="text-xs text-destructive">
						Enter a whole number from {SETUP_KEPT_MIN} to {SETUP_KEPT_MAX}.
					</p>
				{/if}
			</div>
			<div class="grid content-start gap-2">
				<Label for="setup-worlds-kept">Of those, with their world backup</Label>
				<Input
					id="setup-worlds-kept"
					type="number"
					min="0"
					max={SETUP_KEPT_MAX}
					step="1"
					aria-invalid={!worldsValid}
					aria-describedby="setup-worlds-kept-error"
					bind:value={worlds}
				/>
				{#if !worldsValid}
					<p id="setup-worlds-kept-error" class="text-xs text-destructive">
						Enter a whole number from 0 to {SETUP_KEPT_MAX}.
					</p>
				{/if}
			</div>
		</div>
		<p class="text-sm text-muted-foreground">
			The oldest automatic setups go first; lowering a number removes them when you save. A world
			backup an automatic setup keeps stays outside backup retention, so each one adds to disk use.
			Setups you save yourself are never removed.
		</p>
		<Problem error={failure} />
		<SaveFooter
			saveLabel="Save"
			{changed}
			busy={saving}
			invalid={!versionsValid || !worldsValid}
			ondiscard={reset}
			onsave={save}
		/>
	</Card.Content>
</Card.Root>
