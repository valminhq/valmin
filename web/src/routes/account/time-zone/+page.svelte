<script lang="ts">
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { browserZone } from '$lib/api/schedules';
	import type { User } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';

	const browser = browserZone();
	const zones = [...new Set([...Intl.supportedValuesOf('timeZone'), 'UTC'])].sort();

	let chosen = $state(session.user?.timezone ?? '');
	let busy = $state(false);
	let saved = $state(false);
	let failure = $state<unknown>(null);

	const current = $derived(session.user?.timezone ?? '');
	const known = $derived(chosen === '' || zones.includes(chosen));
	const ready = $derived(known && chosen !== current && !busy);

	async function save(zone: string) {
		busy = true;
		saved = false;
		failure = null;
		try {
			session.user = await api.patch<User>('/me', { timezone: zone });
			chosen = zone;
			saved = true;
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (ready) void save(chosen.trim());
	}
</script>

<main class="mx-auto grid max-w-2xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Time zone</h1>
		<p class="text-sm text-muted-foreground">
			The zone the panel shows times in, and the one new schedules run in. Existing schedules keep
			the zone they were created with. It is saved on your account, so it follows you to every
			browser.
		</p>
	</header>

	<Problem error={failure} />

	{#if saved}
		<section
			class="grid gap-1 rounded-lg border border-l-4 border-l-primary bg-muted/40 p-4"
			role="status"
		>
			<h2 class="font-semibold">Time zone saved</h2>
			<p class="text-sm text-muted-foreground">
				Times are now shown in {current || `${browser}, as your browser reports it`}.
			</p>
		</section>
	{/if}

	<Card.Root>
		<form onsubmit={submit}>
			<Card.Content class="grid gap-4">
				<div class="grid gap-2">
					<Label for="time-zone">Time zone</Label>
					<!-- A native datalist: type to filter the zones the browser knows. -->
					<Input
						id="time-zone"
						list="time-zones"
						autocomplete="off"
						placeholder="Follow my browser ({browser})"
						bind:value={chosen}
					/>
					<datalist id="time-zones">
						{#each zones as zone (zone)}
							<option value={zone}></option>
						{/each}
					</datalist>
					{#if !known}
						<p class="text-sm text-destructive">
							Choose a zone from the list, such as Europe/Berlin.
						</p>
					{/if}
					<p class="text-sm text-muted-foreground">
						Your browser reports {browser}. Browsers with fingerprinting protection, such as Firefox
						with resist fingerprinting, LibreWolf or Tor Browser, report Atlantic/Reykjavik or UTC
						instead of your real zone.
					</p>
				</div>
				<div class="flex flex-wrap gap-2">
					<Button type="submit" disabled={!ready}>Save</Button>
					{#if current !== ''}
						<Button type="button" variant="outline" disabled={busy} onclick={() => save('')}>
							Follow my browser
						</Button>
					{/if}
				</div>
			</Card.Content>
		</form>
	</Card.Root>
</main>
