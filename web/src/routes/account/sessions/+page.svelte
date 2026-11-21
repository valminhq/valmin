<script lang="ts">
	import PageHeader from '#lib/components/page-header.svelte';
	import { pageNarrow } from '#lib/layout.js';
	import { ApiError } from '#lib/api/errors.js';
	import { sessions, type DeviceSession } from '#lib/api/sessions.js';
	import { formatInstant } from '#lib/api/schedules.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import EmptyState from '#lib/components/empty-state.svelte';
	import Problem from '#lib/components/problem.svelte';

	let list = $state<DeviceSession[]>([]);
	let loading = $state(true);
	let busy = $state(false);
	let failure = $state<unknown>(null);
	let ending = $state<DeviceSession | null>(null);
	let confirmOpen = $state(false);

	/** Reads the signed-in devices again. */
	async function load() {
		try {
			list = await sessions.list();
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}
	$effect(() => {
		void load();
	});

	/** Signs the confirmed device out and reads the list again. A refusal stays on screen; a
	 * session that is already gone only needs the list read again. */
	async function end() {
		const target = ending;
		if (!target) return;
		busy = true;
		failure = null;
		try {
			await sessions.end(target.id);
		} catch (err) {
			if (!(err instanceof ApiError && err.status === 404)) {
				failure = err;
				return;
			}
		} finally {
			busy = false;
		}
		await load();
	}
</script>

<main class={pageNarrow}>
	<PageHeader title="Signed-in devices">
		The browsers and devices signed in to your account. Sign out any you do not recognise. Changing
		your password signs out every one of them except the one you change it from.
	</PageHeader>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if list.length === 0 && !failure}
		<EmptyState>No signed-in devices were found.</EmptyState>
	{:else}
		<ul class="grid gap-2">
			{#each list as device (device.id)}
				<li class="flex flex-wrap items-center gap-3 rounded-lg border p-3">
					<div class="grid min-w-0 grow basis-64 gap-1">
						<div class="flex flex-wrap items-center gap-2">
							<span class="text-sm font-medium">{device.ip || 'Unknown address'}</span>
							{#if device.current}<Badge variant="secondary">This browser</Badge>{/if}
						</div>
						<p class="text-sm wrap-anywhere text-muted-foreground">
							{device.user_agent || 'Unknown browser'}
						</p>
						<p class="text-sm text-muted-foreground">
							Last active {formatInstant(device.last_seen_at)} · signed in {formatInstant(
								device.created_at
							)}
						</p>
					</div>
					{#if !device.current}
						<Button
							variant="outline"
							size="sm"
							disabled={busy}
							onclick={() => {
								ending = device;
								confirmOpen = true;
							}}
						>
							Sign out
						</Button>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</main>

<DestructiveConfirm
	bind:open={confirmOpen}
	title="Sign this device out?"
	description={ending
		? `${ending.ip || 'This device'} stops being signed in at once and has to sign in again.`
		: ''}
	confirmLabel="Sign out"
	onconfirm={end}
/>
