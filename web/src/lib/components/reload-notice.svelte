<script lang="ts">
	import { updated } from '$app/state';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
</script>

<!-- Shown once the server reports a newer build than the one this tab loaded. -->
{#if updated.current}
	<Alert.Root class="rounded-none border-x-0 border-t-0">
		<RefreshCw />
		<Alert.Title>Valmin was updated</Alert.Title>
		<Alert.Description>Reload this page to use the new version.</Alert.Description>
		<Alert.Action>
			<Button size="sm" onclick={() => location.reload()}>Reload</Button>
		</Alert.Action>
	</Alert.Root>
{/if}
