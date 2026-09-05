<script lang="ts">
	import * as Alert from '$lib/components/ui/alert';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
</script>

<!--
	`restart_required`, shown wherever it can be set (B11). The rebuild is stated conditionally
	because it happens conditionally: a launch field that drifts from the container is rebuilt on
	the next start, a mod install is not (ADR-118, ADR-107).
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
