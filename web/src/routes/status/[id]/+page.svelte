<script lang="ts">
	import { page } from '$app/state';
	import { Button } from '$lib/components/ui/button';

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
	let checkedAt = $state<Date | null>(null);
	let request = 0;

	$effect(() => {
		void load(id);
	});

	async function load(instanceId: string) {
		const current = ++request;
		loading = true;
		try {
			const response = await fetch(`/public/status/${instanceId}`);
			// A server that has not been published and one that does not exist answer the same
			// way, and this screen must not tell them apart either.
			const result = response.ok ? ((await response.json()) as PublicStatus) : null;
			if (current !== request) return;
			missing = !response.ok;
			status = result;
			checkedAt = response.ok ? new Date() : null;
		} catch {
			if (current !== request) return;
			checkedAt = null;
			missing = true;
			status = null;
		} finally {
			if (current === request) loading = false;
		}
	}
</script>

<svelte:head>
	<title>{status ? status.name : 'Server status'}</title>
</svelte:head>

<main class="mx-auto grid max-w-md gap-6 p-6">
	<div class="grid justify-items-start gap-2">
		<Button variant="outline" disabled={loading} onclick={() => load(id)}>
			{loading ? 'Refreshing…' : 'Refresh status'}
		</Button>
		<p class="text-sm text-muted-foreground">This is a snapshot. Refresh to check for changes.</p>
		{#if checkedAt && !loading}
			<p class="text-sm text-muted-foreground">Last checked {checkedAt.toLocaleString()}.</p>
		{/if}
	</div>
	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if missing || !status}
		<p class="text-sm text-muted-foreground">This status page is unavailable.</p>
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
			<p class="text-sm text-muted-foreground">
				{status.observed_at
					? `Player count observed ${new Date(status.observed_at).toLocaleString()}.`
					: 'Player observation time is unavailable.'}
			</p>
		</div>
	{/if}
</main>
