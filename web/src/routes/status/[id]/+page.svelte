<script lang="ts">
	import { formatInstant } from '#lib/api/schedules.js';
	import { page } from '$app/state';
	import { Button } from '#lib/components/ui/button/index.js';
	import CopyButton from '#lib/components/copy-button.svelte';

	/** One server's published status, as the unauthenticated route serves it. Fetched with a
	 * plain request rather than through the API client: this route is not under `/api/v1`, it
	 * carries no session and no CSRF token, and that is the point. */
	interface PublicStatus {
		name: string;
		online: boolean;
		players: number | null;
		observed_at: string | null;
		/** The administrator's plain text, absent when unset. */
		notice?: string;
		connect_info?: string;
		/** Panel work keeping the server down, such as "updating"; absent when there is none. */
		activity?: string;
		/** How to join and the profile code, present only when the administrator shows them. */
		join_address?: string;
		steam_join_url?: string;
		join_code?: string;
		modpack_code?: string;
		modpack_registry?: string;
	}

	/** Two reads a minute, well inside the route's own per-IP limit. */
	const AUTO_REFRESH_MS = 30_000;
	const AUTO_REFRESH_KEY = 'valmin.status.auto-refresh';

	const id = $derived(page.params.id ?? '');

	let status = $state<PublicStatus | null>(null);
	let missing = $state(false);
	let loading = $state(true);
	let checkedAt = $state<Date | null>(null);
	let autoRefresh = $state(readAutoRefresh());
	let request = 0;

	$effect(() => {
		void load(id);
	});

	// Re-reads on a timer while the choice is on, skipping ticks while the tab is hidden.
	$effect(() => {
		if (!autoRefresh) return;
		const timer = setInterval(() => {
			if (document.visibilityState === 'visible' && !loading) void load(id);
		}, AUTO_REFRESH_MS);
		return () => clearInterval(timer);
	});

	/** The remembered choice, off when storage is unavailable. */
	function readAutoRefresh(): boolean {
		try {
			return localStorage.getItem(AUTO_REFRESH_KEY) === 'true';
		} catch {
			return false;
		}
	}

	function setAutoRefresh(on: boolean) {
		autoRefresh = on;
		try {
			localStorage.setItem(AUTO_REFRESH_KEY, String(on));
		} catch {
			// Storage is unavailable; the choice lasts for this visit only.
		}
	}

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
		<label class="flex items-center gap-2 text-sm">
			<input
				type="checkbox"
				class="size-4 accent-primary"
				checked={autoRefresh}
				onchange={(event) => setAutoRefresh(event.currentTarget.checked)}
			/>
			Refresh automatically
		</label>
		<p class="text-sm text-muted-foreground">
			{autoRefresh
				? 'Checks again every 30 seconds while this tab is open.'
				: 'This is a snapshot. Refresh to check for changes.'}
		</p>
		{#if checkedAt && !loading}
			<p class="text-sm text-muted-foreground">Last checked {formatInstant(checkedAt)}.</p>
		{/if}
	</div>
	{#if loading && !status}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if missing || !status}
		<p class="text-sm text-muted-foreground">This status page is unavailable.</p>
	{:else}
		<div class="grid gap-4 rounded-lg border p-6">
			<h1 class="text-lg font-semibold">{status.name}</h1>
			{#if status.notice}
				<p
					class="rounded-md border bg-muted p-3 text-sm break-words whitespace-pre-line"
					data-testid="notice"
				>
					{status.notice}
				</p>
			{/if}
			<p class="flex items-center gap-2 text-sm">
				<span
					class="size-2 rounded-full {status.online
						? 'bg-emerald-500'
						: status.activity
							? 'bg-amber-500'
							: 'bg-muted-foreground'}"
					aria-hidden="true"
				></span>
				{#if status.online}
					Online
				{:else if status.activity}
					Maintenance in progress: {status.activity}
				{:else}
					Offline
				{/if}
			</p>
			<!-- Null is not zero: it means the panel cannot say right now, and saying "nobody is
			     playing" instead would be a claim it has not measured. -->
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
					? `Player count observed ${formatInstant(status.observed_at)}.`
					: 'Player observation time is unavailable.'}
			</p>
			{#if status.connect_info || status.join_address || status.join_code || status.modpack_code}
				<div class="grid gap-3">
					<h2 class="text-sm font-medium">How to join</h2>
					{#if status.modpack_code}
						<div class="grid gap-1 text-sm">
							<p class="text-muted-foreground">
								Install
								<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
								<a
									class="underline underline-offset-4"
									href="https://thunderstore.io/c/valheim/p/Kesomannen/GaleModManager/"
									target="_blank"
									rel="noreferrer">Gale</a
								>{#if status.modpack_registry !== 'hexium'}
									or
									<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
									<a
										class="underline underline-offset-4"
										href="https://thunderstore.io/c/valheim/p/ebkr/r2modman/"
										target="_blank"
										rel="noreferrer">r2modman</a
									>{/if}, create a Valheim profile, and import this profile code:
							</p>
							<div class="flex flex-wrap items-center gap-2">
								<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm break-all select-all"
									>{status.modpack_code}</code
								>
								<CopyButton
									value={status.modpack_code}
									ariaLabel="Copy profile code"
									variant="ghost"
									size="sm"
								/>
							</div>
						</div>
					{/if}
					{#if status.join_code}
						<div class="flex flex-wrap items-center gap-2 text-sm">
							<span class="text-muted-foreground">Join code</span>
							<code
								class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm font-semibold tracking-wider select-all"
								>{status.join_code}</code
							>
							<CopyButton
								value={status.join_code}
								ariaLabel="Copy join code"
								variant="ghost"
								size="sm"
							/>
						</div>
					{/if}
					{#if status.join_address}
						<div class="grid gap-1 text-sm">
							<div class="flex flex-wrap items-center gap-2">
								<span class="text-muted-foreground">Address</span>
								<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm break-all select-all"
									>{status.join_address}</code
								>
								<CopyButton
									value={status.join_address}
									ariaLabel="Copy join address"
									variant="ghost"
									size="sm"
								/>
							</div>
							{#if status.steam_join_url}
								<div>
									<Button variant="outline" size="sm" href={status.steam_join_url}
										>Join with Steam</Button
									>
								</div>
								<p class="text-xs text-muted-foreground">
									Close the game first: the link starts it and connects, and does nothing while the
									game is already running.
								</p>
							{/if}
						</div>
					{/if}
					{#if status.connect_info}
						<p class="text-sm break-words whitespace-pre-line" data-testid="connect-info">
							{status.connect_info}
						</p>
					{/if}
				</div>
			{/if}
		</div>
	{/if}
</main>
