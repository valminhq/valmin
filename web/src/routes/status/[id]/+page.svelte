<script lang="ts">
	import { page } from '$app/state';

	/** One server's published status, as the unauthenticated route serves it. Fetched with a
	 * plain request rather than through the API client: this route is not under `/api/v1`, it
	 * carries no session and no CSRF token, and that is the point. */
	interface PublicStatus {
		name: string;
		online: boolean;
		players: number | null;
		observed_at: string | null;
	}

	const id = $derived(page.params.id ?? '');

	let status = $state<PublicStatus | null>(null);
	let missing = $state(false);
	let loading = $state(true);

	$effect(() => {
		void load(id);
	});

	async function load(instanceId: string) {
		loading = true;
		try {
			const response = await fetch(`/public/status/${instanceId}`);
			// A server that has not been published and one that does not exist answer the same
			// way, and this screen must not tell them apart either.
			missing = !response.ok;
			status = response.ok ? ((await response.json()) as PublicStatus) : null;
		} catch {
			missing = true;
			status = null;
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head>
	<title>{status ? status.name : 'Server status'}</title>
</svelte:head>

<div class="mx-auto grid max-w-md gap-6 p-6">
	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if missing || !status}
		<p class="text-sm text-muted-foreground">There is nothing published here.</p>
	{:else}
		<div class="grid gap-4 rounded-lg border p-6">
			<h1 class="text-lg font-semibold">{status.name}</h1>
			<p class="flex items-center gap-2 text-sm">
				<span
					class="size-2 rounded-full {status.online ? 'bg-emerald-500' : 'bg-muted-foreground'}"
					aria-hidden="true"
				></span>
				{status.online ? 'Online' : 'Offline'}
			</p>
			<!-- Null is not zero: it means the panel cannot say right now, and saying "nobody is
			     playing" instead would be a claim it has not measured (E7). -->
			<p class="text-sm text-muted-foreground">
				{#if status.players === null}
					Player count unknown
				{:else if status.players === 1}
					1 player online
				{:else}
					{status.players} players online
				{/if}
			</p>
		</div>
	{/if}
</div>
