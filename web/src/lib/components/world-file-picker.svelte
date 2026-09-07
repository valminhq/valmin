<script lang="ts">
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';

	let {
		picked = $bindable(),
		allowBackupVariant = $bindable(false),
		disabled = false
	}: {
		picked?: FileList;
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
	<Label for="world-files">World files</Label>
	<Input id="world-files" type="file" multiple {disabled} bind:files={picked} />
	<p class="text-xs text-muted-foreground">
		A world is a pair — the <span class="font-mono">.db</span> and the
		<span class="font-mono">.fwl</span> of the same name. Select both, or one zip containing them.
	</p>
</div>

<!--
	`03 §4.1` rule 5. The game keeps rolling backups beside the live world and the daemon
	refuses them by default, so restoring one is asked rather than inferred.
-->
<div class="flex items-center justify-between gap-4">
	<div class="grid gap-1">
		<Label for="allow-backup-variant">Allow an older rolling backup</Label>
		<p class="text-xs text-muted-foreground">
			The game saves earlier copies of a world beside it. Turn this on only to go back to one of
			those on purpose.
		</p>
	</div>
	<Switch id="allow-backup-variant" {disabled} bind:checked={allowBackupVariant} />
</div>
