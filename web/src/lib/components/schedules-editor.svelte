<script lang="ts">
	import { type Instance } from '$lib/api/instances';
	import { schedules, scheduleKinds, type Schedule } from '$lib/api/schedules';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	let { instance }: { instance: Instance } = $props();

	let list = $state<Schedule[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let saving = $state(false);
	let kind = $state('');
	let deleting = $state<Schedule | null>(null);
	let deleteOpen = $state(false);

	/**
	 * The builder only ever *writes* an expression. Reading one back would mean a second parser
	 * in the SPA, and the daemon's is the one that actually decides when a job runs — so an
	 * existing schedule keeps showing the expression it was created with, and editing one means
	 * writing it out again.
	 */
	type Every = 'hours' | 'day' | 'week' | 'custom';
	let every = $state<Every>('day');
	let atTime = $state('04:00');
	let everyHours = $state('6');
	let weekday = $state('0');
	let custom = $state('0 4 * * *');

	// Only the divisors of 24. A step of 5 would fire at 20:00 and then again at 00:00 four
	// hours later, which is not the even spacing the option claims.
	const hourChoices = ['1', '2', '3', '4', '6', '8', '12'];
	const days = [
		{ value: '0', label: 'Sunday' },
		{ value: '1', label: 'Monday' },
		{ value: '2', label: 'Tuesday' },
		{ value: '3', label: 'Wednesday' },
		{ value: '4', label: 'Thursday' },
		{ value: '5', label: 'Friday' },
		{ value: '6', label: 'Saturday' }
	];

	/** `atTime` is an `<input type="time">`, so it is always `HH:MM` and these are its halves,
	 * taken by position rather than by parsing anything. */
	const hh = $derived(atTime.slice(0, 2));
	const mm = $derived(atTime.slice(3, 5));
	const dayName = $derived(days.find((d) => d.value === weekday)?.label ?? 'Sunday');

	const built = $derived.by(() => {
		if (every === 'hours') return `0 */${everyHours} * * *`;
		if (every === 'week') return `${mm} ${hh} * * ${weekday}`;
		if (every === 'day') return `${mm} ${hh} * * *`;
		return custom.trim();
	});

	/** What the built expression means, said from the controls rather than read back off the
	 * string. Custom has none: the daemon is the only thing that knows what it means. */
	const meaning = $derived.by(() => {
		if (every === 'hours') {
			const n = Number(everyHours);
			const times = Array.from(
				{ length: 24 / n },
				(_, i) => `${String(i * n).padStart(2, '0')}:00`
			);
			return `Every ${n} hour${n === 1 ? '' : 's'} — ${times.join(', ')}`;
		}
		if (every === 'week') return `Every ${dayName} at ${atTime}`;
		if (every === 'day') return `Every day at ${atTime}`;
		return '';
	});

	/** The daemon sends the zone with each row, and every schedule shares it. Absent until one
	 * exists, and then said plainly rather than guessed at from the browser's clock. */
	const zone = $derived(list[0]?.timezone ?? null);

	const allowed = $derived(session.allowed(instance.id));
	/** Each kind is gated on the action its tick would exercise, not on one schedule
	 * capability: a member who may back this server up may schedule a backup of it
	 * (ADR-132). */
	const offered = $derived(scheduleKinds.filter((k) => allowed.includes(k.action)));
	const mine = $derived(list.filter((s) => s.instance_id === instance.id));
	const ready = $derived(kind !== '' && built !== '' && !saving);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			list = await schedules.list();
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function act(call: () => Promise<unknown>) {
		saving = true;
		failure = null;
		try {
			await call();
			await load();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	function create() {
		void act(async () => {
			await schedules.create({ instance_id: instance.id, kind, cron: built });
			kind = '';
		});
	}

	const toggle = (s: Schedule, enabled: boolean) =>
		void act(() => schedules.patch(s.id, { enabled }));

	function remove() {
		const target = deleting;
		if (target) void act(() => schedules.remove(target.id));
	}

	function ask(s: Schedule) {
		deleting = s;
		deleteOpen = true;
	}

	function label(k: string): string {
		return scheduleKinds.find((s) => s.kind === k)?.label ?? k.replaceAll('_', ' ');
	}

	function when(iso: string | null): string {
		return iso ? new Date(iso).toLocaleString() : 'never';
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Schedules</Card.Title>
		<Card.Description>
			Run backups, restarts and game updates automatically at scheduled times. If the server is busy
			when a task is due, that run is skipped.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		{#if offered.length === 0}
			<p class="text-sm text-muted-foreground" data-testid="schedules-blocked">
				Scheduling is not available to you.
			</p>
		{:else}
			<Problem error={failure} />

			{#if loading}
				<p class="text-sm text-muted-foreground">Loading…</p>
			{:else if mine.length === 0}
				<p class="text-sm text-muted-foreground">Nothing is scheduled for this server.</p>
			{:else}
				<div class="grid gap-2">
					{#each mine as s (s.id)}
						<div class="flex flex-wrap items-center gap-3 rounded-lg border p-3">
							<div class="grid flex-1 gap-1">
								<div class="flex flex-wrap items-center gap-2">
									<span class="text-sm font-medium">{label(s.kind)}</span>
									<Badge variant="outline" class="font-mono">{s.cron}</Badge>
									{#if !s.enabled}<Badge variant="secondary">paused</Badge>{/if}
								</div>
								<!--
									The timezone is the daemon's, sent with the row. An operator reading "04:00"
									and assuming their own clock is the misunderstanding this names away.
								-->
								<p class="text-sm text-muted-foreground">
									next {when(s.next_run_at)} · last {when(s.last_run_at)} · times in {s.timezone}
									{#if s.created_by_username}· set up by {s.created_by_username}{/if}
								</p>
							</div>
							<Switch
								checked={s.enabled}
								disabled={saving}
								onCheckedChange={(v) => toggle(s, v)}
								aria-label="Enable {label(s.kind)} schedule"
							/>
							<Button variant="ghost" size="sm" disabled={saving} onclick={() => ask(s)}>
								<Trash2 />
								<span class="sr-only">Delete schedule</span>
							</Button>
						</div>
					{/each}
				</div>
			{/if}

			<div class="grid gap-3 border-t pt-4">
				<div class="grid gap-2">
					<Label for="schedule-kind">What to run</Label>
					<Select.Root type="single" bind:value={kind}>
						<Select.Trigger id="schedule-kind">
							{kind ? label(kind) : 'Choose…'}
						</Select.Trigger>
						<Select.Content>
							{#each offered as option (option.kind)}
								<Select.Item value={option.kind}>{option.label}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				</div>
				<div class="grid gap-2">
					<Label for="schedule-every">How often</Label>
					<div class="flex flex-wrap items-end gap-2">
						<Select.Root type="single" bind:value={every}>
							<Select.Trigger id="schedule-every" class="w-44">
								{every === 'hours'
									? 'Every few hours'
									: every === 'day'
										? 'Every day'
										: every === 'week'
											? 'Every week'
											: 'Cron expression'}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value="hours">Every few hours</Select.Item>
								<Select.Item value="day">Every day</Select.Item>
								<Select.Item value="week">Every week</Select.Item>
								<Select.Item value="custom">Cron expression</Select.Item>
							</Select.Content>
						</Select.Root>

						{#if every === 'hours'}
							<Select.Root type="single" bind:value={everyHours}>
								<Select.Trigger class="w-32" aria-label="Hours between runs">
									{everyHours} hours
								</Select.Trigger>
								<Select.Content>
									{#each hourChoices as h (h)}
										<Select.Item value={h}>{h} hours</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						{/if}

						{#if every === 'week'}
							<Select.Root type="single" bind:value={weekday}>
								<Select.Trigger class="w-36" aria-label="Day of the week">{dayName}</Select.Trigger>
								<Select.Content>
									{#each days as d (d.value)}
										<Select.Item value={d.value}>{d.label}</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						{/if}

						{#if every === 'day' || every === 'week'}
							<!-- Native time input: a locale-correct picker with no library behind it. -->
							<Input
								type="time"
								class="w-32"
								bind:value={atTime}
								aria-label="Time of day"
								step="60"
							/>
						{/if}
					</div>
				</div>

				{#if every === 'custom'}
					<div class="grid gap-2">
						<Label for="schedule-cron">Cron expression</Label>
						<!--
							The expression is the daemon's to validate: it answers an invalid one with the
							field and its own help text, so there is no second, weaker parser here.
						-->
						<Input
							id="schedule-cron"
							bind:value={custom}
							autocomplete="off"
							placeholder="0 4 * * *"
							class="font-mono"
						/>
						<p class="text-sm text-muted-foreground">
							Five fields: minute, hour, day of month, month, day of week. Shorthands such as
							<span class="font-mono">@daily</span> work too.
						</p>
					</div>
				{:else}
					<!--
						The generated expression stays visible rather than hidden behind the controls: it is
						what the schedule list shows afterwards, and an operator who wants to write one by
						hand next time can read it here.
					-->
					<p class="text-sm text-muted-foreground">
						{meaning} · <span class="font-mono">{built}</span>
					</p>
				{/if}

				<p class="text-sm text-muted-foreground">
					{#if zone}
						Times are the server’s, in {zone} — not your own clock.
					{:else}
						Times are the server’s, not your own clock.
					{/if}
				</p>

				<Button size="sm" class="justify-self-start" disabled={!ready} onclick={create}>
					Add schedule
				</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>

<!-- F5: deleting a schedule stops something running unattended, so it is named and typed back. -->
<DestructiveConfirm
	bind:open={deleteOpen}
	name={deleting ? deleting.cron : ''}
	title="Delete this schedule?"
	description="{deleting
		? label(deleting.kind)
		: ''} stops running on its own. Runs already in the job history are kept."
	confirmLabel="Delete"
	onconfirm={remove}
/>
