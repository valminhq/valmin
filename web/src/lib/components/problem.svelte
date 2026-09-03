<script lang="ts">
	import { ApiError, NetworkError } from '$lib/api/errors';
	import * as Alert from '$lib/components/ui/alert';

	let { error }: { error: unknown } = $props();

	const api = $derived(error instanceof ApiError ? error : null);
	const network = $derived(error instanceof NetworkError ? error : null);
	const message = $derived(
		api?.message ?? network?.message ?? (error ? 'Something went wrong.' : '')
	);
</script>

<!--
	The request id is shown, always. The message is generic by design (D10, `11 §2.1`) —
	the wrapped chain that explains it is in the daemon's log under this id — so an operator
	reporting "it said something went wrong" can be answered instead of guessed at.
-->
{#if error}
	<Alert.Root variant="destructive">
		<Alert.Title>{message}</Alert.Title>
		{#if api?.requestId}
			<Alert.Description>
				<span class="font-mono text-xs">request {api.requestId}</span>
			</Alert.Description>
		{/if}
	</Alert.Root>
{/if}
