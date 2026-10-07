<script lang="ts">
	import { resolve } from '$app/paths';
	import { type Instance } from '$lib/api/instances';
	import {
		deferral,
		clockChanges,
		deferralChoices,
		inClockChange,
		inZone,
		kindLabel,
		schedules,
		scheduleKinds,
		viewerZone,
		type Schedule
	} from '$lib/api/schedules';
	import { session } from '$lib/state/session.svelte';
	import { socket } from '$lib/socket/index.svelte';
	import { topics } from '$lib/socket/messages';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import * as Dialog from '$lib/components/ui/dialog';
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
	type Every = 'hours' | 'day' | 'days' | 'month' | 'custom';
	let every = $state<Every>('day');
	let atTime = $state('04:00');
	let startAt = $state('00:00');
	let everyHours = $state('6');
	let weekdays = $state<string[]>(['0']);
	let monthDay = $state('1');
	let custom = $state('0 4 * * *');
	let waitForEmpty = $state(false);
	let maxDeferral = $state('7200');
	let unknownPlayers = $state<'wait' | 'run'>('wait');

	const everyLabels: Record<Every, string> = {
		hours: 'Every few hours',
		day: 'Every day',
		days: 'On days of the week',
		month: 'Every month',
		custom: 'Cron expression'
	};

	// Only the divisors of 24. A step of 5 would fire at 20:00 and then again at 00:00 four
	// hours later, which is not the even spacing the option claims.
	const hourChoices = ['1', '2', '3', '4', '6', '8', '12'];
	// Listed Monday first; the values are cron's, where Sunday is 0.
	const days = [
		{ value: '1', label: 'Monday' },
		{ value: '2', label: 'Tuesday' },
		{ value: '3', label: 'Wednesday' },
		{ value: '4', label: 'Thursday' },
		{ value: '5', label: 'Friday' },
		{ value: '6', label: 'Saturday' },
		{ value: '0', label: 'Sunday' }
	];
	// Days 29 to 31 are left out: they do not occur in every month, so such a schedule would
	// silently skip some months.
	const monthDays = Array.from({ length: 28 }, (_, i) => String(i + 1));

	/** `atTime` and `startAt` are `<input type="time">`, so each is always `HH:MM` and these are
	 * its halves, taken by position rather than by parsing anything. */
	const hh = $derived(atTime.slice(0, 2));
	const mm = $derived(atTime.slice(3, 5));
	/** The first hour of an every-few-hours run, folded into the first step of the day. */
	const firstHour = $derived(Number(startAt.slice(0, 2)) % Number(everyHours));
	const startMinute = $derived(startAt.slice(3, 5));
	const chosenDays = $derived(days.filter((d) => weekdays.includes(d.value)));

	const built = $derived.by(() => {
		if (every === 'hours')
			return `${Number(startMinute)} ${firstHour === 0 ? '*' : firstHour}/${everyHours} * * *`;
		if (every === 'days') {
			if (weekdays.length === 0) return '';
			const values = [...weekdays].sort((a, b) => Number(a) - Number(b)).join(',');
			return `${mm} ${hh} * * ${weekdays.length === 7 ? '*' : values}`;
		}
		if (every === 'month') return `${mm} ${hh} ${monthDay} * *`;
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
				(_, i) => `${String(firstHour + i * n).padStart(2, '0')}:${startMinute}`
			);
			return `Every ${n} hour${n === 1 ? '' : 's'} — ${times.join(', ')}`;
		}
		if (every === 'days') {
			if (chosenDays.length === 0) return 'Choose at least one day';
			if (chosenDays.length === 7) return `Every day at ${atTime}`;
			return `Every ${joinWords(chosenDays.map((d) => d.label))} at ${atTime}`;
		}
		if (every === 'month') return `Every month on the ${ordinal(Number(monthDay))} at ${atTime}`;
		if (every === 'day') return `Every day at ${atTime}`;
		return '';
	});

	function joinWords(words: string[]): string {
		return words.length < 2
			? words.join('')
			: `${words.slice(0, -1).join(', ')} and ${words.at(-1)}`;
	}

	function ordinal(n: number): string {
		const tens = n % 100;
		const suffix = tens >= 11 && tens <= 13 ? 'th' : (['th', 'st', 'nd', 'rd'][n % 10] ?? 'th');
		return `${n}${suffix}`;
	}

	function toggleDay(value: string) {
		weekdays = weekdays.includes(value)
			? weekdays.filter((d) => d !== value)
			: [...weekdays, value];
	}

	let zone = $state<string | null>(null);
	const viewer = $derived(viewerZone());

	/** The local times of day, in minutes, the built expression runs at. None for a written
	 * expression: only the daemon knows what that one means. */
	const runMinutes = $derived.by(() => {
		if (every === 'custom') return [];
		if (every !== 'hours') return [Number(hh) * 60 + Number(mm)];
		const n = Number(everyHours);
		return Array.from({ length: 24 / n }, (_, i) => (firstHour + i * n) * 60 + Number(startMinute));
	});
	/** The clock changes of the viewer's zone that land on one of those times. */
	const changes = $derived(clockChanges(viewer));
	const clashes = $derived(
		changes.filter((c) => runMinutes.some((minute) => inClockChange(minute, c)))
	);

	/** What the clashing clock changes do to the run, one sentence per change. */
	const clashNote = $derived(
		[...clashes]
			.sort((a, b) => Number(b.kind === 'skipped') - Number(a.kind === 'skipped'))
			.map((c) =>
				c.kind === 'skipped'
					? `The run is skipped on the day the clocks go forward over ${clock(c.start)}–${clock(c.end)}.`
					: `The run happens twice on the day the clocks go back over ${clock(c.start)}–${clock(c.end)}.`
			)
			.join(' ')
	);

	function clock(minute: number): string {
		const m = minute % 1440;
		return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
	}

	const allowed = $derived(session.allowed(instance.id));
	/** Each kind is gated on the action its tick would exercise, not on one schedule
	 * capability: a member who may back this server up may schedule a backup of it
	 * (ADR-132). */
	const offered = $derived(scheduleKinds.filter((k) => allowed.includes(k.action)));
	const mine = $derived(list.filter((s) => s.instance_id === instance.id));
	const ready = $derived(kind !== '' && built !== '' && !!zone && !saving);
	const playerAware = $derived(scheduleKinds.find((k) => k.kind === kind)?.playerAware ?? false);

	/** One entry per enabled schedule, so a frequent schedule cannot crowd the others out:
	 * held runs first, then each schedule's next run, earliest first, with the few after it.
	 * Pausing a schedule releases its hold, so a held run always belongs to an enabled one. */
	const upcoming = $derived.by(() =>
		mine
			.filter((s) => s.enabled)
			.flatMap((s) => {
				const held = s.deferred_since && s.deferred_until ? s.deferred_until : null;
				const at = held ?? s.upcoming_runs[0];
				if (!at) return [];
				const later = held ? s.upcoming_runs : s.upcoming_runs.slice(1);
				return [{ s, at, held: held !== null, later: later.slice(0, 3) }];
			})
			.sort((a, b) => Number(b.held) - Number(a.held) || Date.parse(a.at) - Date.parse(b.at))
	);

	$effect(() => {
		void load();
	});

	$effect(() => {
		const off = socket.subscribe(topics.state(instance.id), (m) => {
			if (m.type === 'maintenance') void load();
		});
		const unhook = socket.onConnected(() => void load());
		// A run that fires or is skipped sends no message, so upcoming times are re-read each minute.
		const tick = setInterval(() => void load(), 60_000);
		return () => {
			off();
			unhook();
			clearInterval(tick);
		};
	});

	async function load() {
		try {
			const page = await schedules.list();
			list = page.items;
			zone = page.timezone;
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
			await schedules.create({
				instance_id: instance.id,
				kind,
				cron: built,
				timezone: viewer,
				...(playerAware
					? {
							wait_for_empty: waitForEmpty,
							max_deferral_seconds: Number(maxDeferral),
							unknown_players: unknownPlayers
						}
					: {})
			});
			kind = '';
		});
	}

	const toggle = (s: Schedule, enabled: boolean) =>
		void act(() => schedules.patch(s.id, { enabled }));

	function remove() {
		const target = deleting;
		deleteOpen = false;
		if (target) void act(() => schedules.remove(target.id));
	}

	function ask(s: Schedule) {
		deleting = s;
		deleteOpen = true;
	}

	const label = kindLabel;

	function when(iso: string | null): string {
		return iso ? inZone(iso, viewer) : 'never';
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

			{#if !loading && mine.length > 0}
				<section class="grid gap-2" aria-labelledby="upcoming-runs">
					<h3 id="upcoming-runs" class="text-sm font-medium">Upcoming runs</h3>
					{#if upcoming.length === 0}
						<p class="text-sm text-muted-foreground">No runs are coming up.</p>
					{:else}
						<ul class="grid gap-2">
							{#each upcoming as run (`${run.s.id} ${run.at} ${run.held}`)}
								<li class="grid gap-0.5 rounded-lg border px-3 py-2 text-sm">
									<span class="font-medium">{label(run.s.kind)}</span>
									{#if run.held}
										<span>
											Waiting for players to leave. Runs at {inZone(run.at, viewer)}
											{viewer} at the latest.
										</span>
									{:else}
										<span>{inZone(run.at, viewer)} {viewer}</span>
									{/if}
									{#if run.later.length > 0}
										<span class="text-muted-foreground">
											Then {run.later.map((t) => inZone(t, viewer)).join(', ')}
										</span>
									{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</section>
			{/if}

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
									{#if s.deferred_since}<Badge>waiting for players</Badge>{/if}
								</div>
								<p class="text-sm text-muted-foreground">
									next {when(s.next_run_at)} · last {when(s.last_run_at)} · shown in {viewer} · schedule
									uses {s.timezone}
									{#if s.created_by_username}· set up by {s.created_by_username}{/if}
									{#if s.wait_for_empty}· waits up to {deferral(s.max_deferral_seconds)} for players to
										leave{/if}
								</p>
								{#if s.deferred_since}
									<p class="text-sm">
										Waiting since {when(s.deferred_since)}. Runs when no players are connected, or
										at
										{when(s.deferred_until)}
										{viewer} at the latest.
									</p>
								{/if}
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
							<Select.Trigger id="schedule-every" class="w-48">{everyLabels[every]}</Select.Trigger>
							<Select.Content>
								{#each Object.entries(everyLabels) as [value, text] (value)}
									<Select.Item {value}>{text}</Select.Item>
								{/each}
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
							<Input
								type="time"
								class="w-32"
								bind:value={startAt}
								aria-label="Starting at"
								step="60"
							/>
						{/if}

						{#if every === 'month'}
							<Select.Root type="single" bind:value={monthDay}>
								<Select.Trigger class="w-36" aria-label="Day of the month">
									on the {ordinal(Number(monthDay))}
								</Select.Trigger>
								<Select.Content>
									{#each monthDays as d (d)}
										<Select.Item value={d}>{ordinal(Number(d))}</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						{/if}

						{#if every === 'day' || every === 'days' || every === 'month'}
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
					{#if every === 'days'}
						<div class="flex flex-wrap gap-1" role="group" aria-label="Days of the week">
							{#each days as d (d.value)}
								{@const on = weekdays.includes(d.value)}
								<Button
									size="sm"
									variant={on ? 'default' : 'outline'}
									aria-pressed={on}
									aria-label={d.label}
									onclick={() => toggleDay(d.value)}
								>
									{d.label.slice(0, 3)}
								</Button>
							{/each}
						</div>
					{/if}
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
							<span class="font-mono">@daily</span> work too. Once added, its next runs appear under Upcoming
							runs.
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
					{#if clashes.length > 0}
						<p class="text-sm text-amber-700 dark:text-amber-400" data-testid="clock-change">
							{viewer} changes its clocks at this time. {clashNote} Choose another time to avoid it.
						</p>
					{/if}
				{/if}

				<p class="text-sm text-muted-foreground">
					{#if zone}
						New schedules run in {viewer}. Existing schedules keep their original zone. Times on
						this page are shown in {viewer}.
						<a class="underline" href={resolve('/account/time-zone')}>Change time zone</a>
					{:else}
						Scheduler timezone is unavailable. Reload this page before creating a schedule.
					{/if}
				</p>

				{#if playerAware}
					<div class="flex items-center justify-between gap-4">
						<Label for="schedule-wait">Wait until no players are connected</Label>
						<Switch id="schedule-wait" bind:checked={waitForEmpty} />
					</div>
					{#if waitForEmpty}
						<div class="grid gap-4 sm:grid-cols-2">
							<div class="grid gap-2">
								<Label for="schedule-max-deferral">Wait at most</Label>
								<Select.Root type="single" bind:value={maxDeferral}>
									<Select.Trigger id="schedule-max-deferral" class="w-full">
										{deferral(Number(maxDeferral))}
									</Select.Trigger>
									<Select.Content>
										{#each deferralChoices as seconds (seconds)}
											<Select.Item value={String(seconds)}>{deferral(seconds)}</Select.Item>
										{/each}
									</Select.Content>
								</Select.Root>
							</div>
							<div class="grid gap-2">
								<Label for="schedule-unknown">If the player count is unknown</Label>
								<Select.Root type="single" bind:value={unknownPlayers}>
									<Select.Trigger id="schedule-unknown" class="w-full">
										{unknownPlayers === 'wait'
											? 'Wait, as if players are connected'
											: 'Run, as if the server is empty'}
									</Select.Trigger>
									<Select.Content>
										<Select.Item value="wait">Wait, as if players are connected</Select.Item>
										<Select.Item value="run">Run, as if the server is empty</Select.Item>
									</Select.Content>
								</Select.Root>
							</div>
						</div>
						<p class="text-sm text-muted-foreground">
							Valmin reads the player count from the server log. It can be unknown for up to 10
							minutes after the server starts. With the Tristan-ValheimRcon mod installed, players
							are warned in chat when the run starts waiting and 5 minutes before the latest time.
						</p>
					{/if}
				{/if}

				<Button size="sm" class="justify-self-start" disabled={!ready} onclick={create}>
					Add schedule
				</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>

<Dialog.Root bind:open={deleteOpen}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Delete this schedule?</Dialog.Title>
			<Dialog.Description>
				{deleting ? label(deleting.kind) : ''}
				<span class="font-mono">({deleting?.cron})</span> stops running on its own. Runs already in the
				job history are kept.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (deleteOpen = false)}>Cancel</Button>
			<Button variant="destructive" onclick={remove}>Delete schedule</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
