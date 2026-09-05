<script lang="ts">
	import * as Alert from '$lib/components/ui/alert';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
</script>

<!--
	B11: `restart_required` has to be obvious wherever it can be set, and it is set from three
	screens. One component so the sentence is corrected in one place.

	The rebuild is stated conditionally because it happens conditionally (ADR-118): a launch
	field that drifts from the container is rebuilt on the next start, and a mod install is
	not — ADR-107 still holds. Promising a rebuild here would be wrong on the config screens.
-->
<Alert.Root>
	<TriangleAlert />
	<Alert.Title>Restart required</Alert.Title>
	<Alert.Description>
		Settings changed since this server started, and the running server is still using the old ones.
		It picks these up the next time it starts, rebuilding its container first if its launch settings
		are among what changed.
	</Alert.Description>
</Alert.Root>
