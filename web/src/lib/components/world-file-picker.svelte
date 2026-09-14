<script lang="ts">
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';

	let {
		picked = $bindable(),
		pickedFolder = $bindable(),
		allowBackupVariant = $bindable(false),
		disabled = false
	}: {
		picked?: FileList;
		pickedFolder?: FileList;
		allowBackupVariant?: boolean;
		disabled?: boolean;
	} = $props();
</script>

<!--
	Deliberately no `accept` filter: what counts as a world is the daemon's rule, and a
	two-extension picker hides files that rule also accepts, the game's `.old` variants
	among them (F2, `03 §4.1`).
-->
<div class="grid gap-2">
	<Label for="world-folder">World folder</Label>
	<!--
		`webkitdirectory` is not in the HTML spec and is what every browser implements, so it
		is spread rather than written as an attribute. The folder is the unit because a
		Valheim 1.0 world *is* a directory, and its name is the world's name (`03 §4`).
	-->
	<Input
		id="world-folder"
		type="file"
		{disabled}
		bind:files={pickedFolder}
		{...{ webkitdirectory: true, directory: true }}
	/>
	<p class="text-xs text-muted-foreground">
		Valheim saves a world as a folder named after it. Pick that folder — it is under
		<span class="font-mono">worlds_local</span>.
	</p>
</div>

<div class="grid gap-2">
	<Label for="world-files">Or files</Label>
	<Input id="world-files" type="file" multiple {disabled} bind:files={picked} />
	<p class="text-xs text-muted-foreground">
		A zip of that folder works too. Older Valheim saved a world as a pair instead — the
		<span class="font-mono">.db</span> and the <span class="font-mono">.fwl</span> of the same name —
		and both files together are that world.
	</p>
</div>

<!--
	`03 §4.1` rule 5. The game keeps rolling backups beside the live world and the daemon
	refuses them by default, so restoring one is asked rather than inferred.
-->
<div class="flex items-center justify-between gap-4">
	<div class="grid gap-1">
		<Label for="allow-backup-variant">Import an older game backup</Label>
		<p class="text-xs text-muted-foreground">
			Enable this if you selected an older backup saved by the game, such as .old files. Importing
			it restores the world to that earlier save.
		</p>
	</div>
	<Switch id="allow-backup-variant" {disabled} bind:checked={allowBackupVariant} />
</div>
