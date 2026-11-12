<script lang="ts">
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import PageHeader from '#lib/components/page-header.svelte';
	import { pageNarrow } from '#lib/layout.js';
	import { api } from '#lib/api/client.js';
	import { browserZone, formatInstant } from '#lib/api/schedules.js';
	import type { User } from '#lib/api/types.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import Problem from '#lib/components/problem.svelte';

	const browser = browserZone();
	const zones = [...new Set([...Intl.supportedValuesOf('timeZone'), 'UTC'])].sort();

	const sample = Date.UTC(2026, 11, 31, 21, 5);
	const hourCycles = [
		{ value: 'h23', label: '24-hour' },
		{ value: 'h12', label: '12-hour' }
	] as const;
	const dateOrders = [
		{ value: 'dmy', label: 'Day, month, year' },
		{ value: 'mdy', label: 'Month, day, year' },
		{ value: 'ymd', label: 'Year, month, day' }
	] as const;

	let chosen = $state(session.user?.timezone ?? '');
	let hourCycle = $state<User['hour_cycle']>(session.user?.hour_cycle ?? '');
	let dateOrder = $state<User['date_order']>(session.user?.date_order ?? '');
	let busy = $state(false);
	let saved = $state<'zone' | 'format' | null>(null);
	let failure = $state<unknown>(null);

	const current = $derived(session.user?.timezone ?? '');
	const known = $derived(chosen === '' || zones.includes(chosen));
	const ready = $derived(known && chosen !== current && !busy);
	const formatChanged = $derived(
		hourCycle !== (session.user?.hour_cycle ?? '') || dateOrder !== (session.user?.date_order ?? '')
	);

	// On a direct load the session arrives after the page; the form then takes the saved values.
	let seeded = session.user !== null;
	$effect(() => {
		if (seeded || !session.user) return;
		seeded = true;
		chosen = session.user.timezone ?? '';
		hourCycle = session.user.hour_cycle ?? '';
		dateOrder = session.user.date_order ?? '';
	});

	unsaved(() => session.user !== null && (chosen !== current || formatChanged));

	const timeOnly = { hour: 'numeric', minute: '2-digit' } as const;
	const dateOnly = { year: 'numeric', month: '2-digit', day: '2-digit' } as const;

	/** How the sample reads in a format, so a choice can be seen before it is saved. */
	function example(
		hour_cycle: User['hour_cycle'],
		date_order: User['date_order'],
		options: Intl.DateTimeFormatOptions = { ...dateOnly, ...timeOnly }
	): string {
		return formatInstant(sample, { timeZone: 'UTC', ...options }, { hour_cycle, date_order });
	}

	async function save(body: Partial<User>, what: 'zone' | 'format') {
		busy = true;
		saved = null;
		failure = null;
		try {
			session.user = await api.patch<User>('/me', body);
			chosen = session.user.timezone;
			saved = what;
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (ready) void save({ timezone: chosen.trim() }, 'zone');
	}

	function submitFormat(event: SubmitEvent) {
		event.preventDefault();
		if (formatChanged && !busy)
			void save({ hour_cycle: hourCycle, date_order: dateOrder }, 'format');
	}
</script>

<main class={pageNarrow}>
	<PageHeader title="Date and time">
		The zone and format the panel shows times in. They are saved on your account, so they follow you
		to every browser.
	</PageHeader>

	<Problem error={failure} />

	{#if saved === 'zone'}
		<section
			class="grid gap-1 rounded-lg border border-l-4 border-l-primary bg-muted/40 p-4"
			role="status"
		>
			<h2 class="font-semibold">Time zone saved</h2>
			<p class="text-sm text-muted-foreground">
				Times are now shown in {current || `${browser}, as your browser reports it`}.
			</p>
		</section>
	{:else if saved === 'format'}
		<section
			class="grid gap-1 rounded-lg border border-l-4 border-l-primary bg-muted/40 p-4"
			role="status"
		>
			<h2 class="font-semibold">Format saved</h2>
			<p class="text-sm text-muted-foreground">
				Times are now shown as {example(hourCycle, dateOrder)}.
			</p>
		</section>
	{/if}

	<Card.Root>
		<form onsubmit={submit}>
			<Card.Content class="grid gap-4">
				<p class="text-sm text-muted-foreground">
					The zone new schedules run in, too. Existing schedules keep the zone they were created
					with.
				</p>
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
						<Button
							type="button"
							variant="outline"
							disabled={busy}
							onclick={() => save({ timezone: '' }, 'zone')}
						>
							Follow my browser
						</Button>
					{/if}
				</div>
			</Card.Content>
		</form>
	</Card.Root>

	<Card.Root>
		<form onsubmit={submitFormat}>
			<Card.Content class="grid gap-4">
				<div class="grid gap-2">
					<Label for="hour-cycle">Clock</Label>
					<NativeSelect id="hour-cycle" bind:value={hourCycle}>
						<option value="">Follow my browser ({example('', '', timeOnly)})</option>
						{#each hourCycles as option (option.value)}
							<option value={option.value}
								>{option.label} ({example(option.value, '', timeOnly)})</option
							>
						{/each}
					</NativeSelect>
				</div>
				<div class="grid gap-2">
					<Label for="date-order">Date</Label>
					<NativeSelect id="date-order" bind:value={dateOrder}>
						<option value="">Follow my browser ({example('', '', dateOnly)})</option>
						{#each dateOrders as option (option.value)}
							<option value={option.value}
								>{option.label} ({example('', option.value, dateOnly)})</option
							>
						{/each}
					</NativeSelect>
				</div>
				<p class="text-sm text-muted-foreground">
					Times will read like {example(hourCycle, dateOrder)}.
				</p>
				<div>
					<Button type="submit" disabled={!formatChanged || busy}>Save format</Button>
				</div>
			</Card.Content>
		</form>
	</Card.Root>
</main>
