<script lang="ts">
	import * as Alert from '$lib/components/ui/alert';
	import type { Instance } from '$lib/api/instances';
	import Info from '@lucide/svelte/icons/info';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let { instance }: { instance: Pick<Instance, 'restart_required' | 'pending_restart'> } = $props();
</script>

<!--
	`restart_required`, shown wherever it can be set (B11). The rebuild is stated conditionally
	because it happens conditionally: a launch field that drifts from the container is rebuilt on
	the next start, a mod install is not (ADR-118, ADR-107). `pending_restart` is the operator's
	own config or mod change, so it is information rather than a warning.
-->
{#if instance.restart_required}
	<Alert.Root>
		<TriangleAlert />
		<Alert.Title>Restart to apply settings</Alert.Title>
		<Alert.Description>
			The running server is using the previous settings. Restart the server to apply your changes.
			Players will be disconnected during the restart. If launch settings changed, Valmin rebuilds
			the container before starting the server.
		</Alert.Description>
	</Alert.Root>
{:else if instance.pending_restart}
	<Alert.Root>
		<Info />
		<Alert.Title>Changes apply on next start</Alert.Title>
		<Alert.Description>
			Mod or config changes apply the next time this server starts. Players are disconnected during
			a restart.
		</Alert.Description>
	</Alert.Root>
{/if}
