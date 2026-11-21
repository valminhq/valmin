<script lang="ts">
	import { tick } from 'svelte';
	import { resolve } from '$app/paths';
	import { type Instance } from '#lib/api/instances.js';
	import {
		buildCron,
		chainSteps,
		deferral,
		defaultCronForm,
		describeSteps,
		clockChanges,
		deferralChoices,
		firstHourOf,
		describeCron,
		describeExpression,
		hourChoices,
		inClockChange,
		inZone,
		kindLabel,
		maxChainSteps,
		maxWaitSeconds,
		monthDays,
		ordinal,
		readCron,
		schedules,
		scheduleKinds,
		stopsServer,
		viewerZone,
		weekdayChoices,
		type ChainStep,
		type ChainStepType,
		type CronForm,
		type Every,
		type PatchSchedule,
		type Schedule,
		type WaitCondition,
		waitConditions
	} from '#lib/api/schedules.js';
	import { session } from '#lib/state/session.svelte.js';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { topics } from '#lib/socket/messages.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import * as Select from '#lib/components/ui/select/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import Problem from '#lib/components/problem.svelte';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import Pencil from '@lucide/svelte/icons/pencil';
	import ArrowUp from '@lucide/svelte/icons/arrow-up';
	import ArrowDown from '@lucide/svelte/icons/arrow-down';
	import Plus from '@lucide/svelte/icons/plus';
	import Play from '@lucide/svelte/icons/play';
	import { warningText } from '#lib/tone.js';

	let { instance }: { instance: Instance } = $props();

	let list = $state<Schedule[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let saving = $state(false);
	let kind = $state('');
	let deleting = $state<Schedule | null>(null);
	let deleteOpen = $state(false);

	/**
	 * The builder's controls, which write the expression. An existing expression is read back
	 * into them only through readCron, which accepts nothing the builder could not have produced;
	 * anything else opens as a written expression.
	 */
	let form = $state<CronForm>(defaultCronForm());
	let waitForEmpty = $state(false);
	let maxDeferral = $state('7200');
	let unknownPlayers = $state<'wait' | 'run'>('wait');
	/** Whether an update test schedule applies what it tested. */
	let applyTested = $state(false);

	const everyLabels: Record<Every, string> = {
		hours: 'Every few hours',
		day: 'Every day',
		days: 'On days of the week',
		month: 'Every month',
		custom: 'Cron expression'
	};

	/** `atTime` and `startAt` are `<input type="time">`, so each is always `HH:MM` and these are
	 * its halves, taken by position. */
	const hh = $derived(form.atTime.slice(0, 2));
	const mm = $derived(form.atTime.slice(3, 5));
	/** The first hour of an every-few-hours run, folded into the first step of the day. */
	const firstHour = $derived(firstHourOf(form));
	const startMinute = $derived(form.startAt.slice(3, 5));

	const built = $derived(buildCron(form));

	/** What the built expression means, said from the controls rather than read back off the
	 * string. */
	const meaning = $derived(describeCron(form));

	function toggleDay(value: string) {
		form.weekdays = form.weekdays.includes(value)
			? form.weekdays.filter((d) => d !== value)
			: [...form.weekdays, value];
	}

	let zone = $state<string | null>(null);
	const viewer = $derived(viewerZone());

	/** The schedule being edited as it was when Edit was chosen, or null while adding one. The
	 * form is filled from this copy once, so a reload of the list never overwrites the form. */
	let editBase = $state<Schedule | null>(null);
	/** Moves the schedule being edited to the viewer's zone. */
	let moveZone = $state(false);
	/** The zone the form's times are on: the edited schedule's own unless it is being moved. */
	const runZone = $derived(editBase && !moveZone ? editBase.timezone : viewer);

	/** The local times of day, in minutes, the built expression runs at. None for a written
	 * expression: only the daemon knows what that one means. */
	const runMinutes = $derived.by(() => {
		if (form.every === 'custom') return [];
		if (form.every !== 'hours') return [Number(hh) * 60 + Number(mm)];
		const n = Number(form.everyHours);
		return Array.from({ length: 24 / n }, (_, i) => (firstHour + i * n) * 60 + Number(startMinute));
	});
	/** The clock changes of the run zone that land on one of those times. */
	const changes = $derived(clockChanges(runZone));
	const clashes = $derived(
		changes.filter((c) => runMinutes.some((minute) => inClockChange(minute, c)))
	);

	/** What the clashing clock changes do to the run, one sentence per change. */
	const clashNote = $derived(
		[...clashes]
			.sort((a, b) => Number(b.kind === 'skipped') - Number(a.kind === 'skipped'))
			.map((c) =>
				c.kind === 'skipped'
					? `On the day the clocks go forward over ${clock(c.start)}–${clock(c.end)}, the run happens once, when the clocks jump.`
					: `On the day the clocks go back over ${clock(c.start)}–${clock(c.end)}, the run happens once, the first time the clock reaches it.`
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

	/** A chain's steps as being written. Wait seconds stay text so the input can be empty. */
	type StepDraft = {
		id: number;
		type: ChainStepType;
		command: string;
		seconds: string;
		condition: WaitCondition;
	};
	let draft = $state<StepDraft[]>([]);
	let nextStepId = 0;
	/** The edited schedule's steps as toStep writes them, so an untouched list reads as unchanged. */
	let baseSteps = $state('');
	let formHeading = $state<HTMLElement | null>(null);

	const effectiveKind = $derived(editBase?.kind ?? kind);
	const isChain = $derived(effectiveKind === 'chain');
	const isUpdateTest = $derived(effectiveKind === 'update_test');
	/** Each step is offered to whoever may do it by hand; the daemon checks it again on write. */
	const offeredSteps = $derived(
		chainSteps.filter((c) => c.action === null || allowed.includes(c.action))
	);

	/** Whether the viewer may change s: its kind is offered and they may run each of its steps. */
	function canEdit(s: Schedule): boolean {
		return (
			offered.some((k) => k.kind === s.kind) &&
			(s.steps ?? []).every((st) => offeredSteps.some((o) => o.type === st.type))
		);
	}

	function toDraft(s: ChainStep): StepDraft {
		return {
			id: nextStepId++,
			type: s.type,
			command: s.command ?? '',
			seconds: String(s.seconds ?? 60),
			condition: s.condition ?? 'ready'
		};
	}

	function toStep(d: StepDraft): ChainStep {
		if (d.type === 'command') return { type: d.type, command: d.command.trim() };
		if (d.type === 'wait') return { type: d.type, seconds: Number(d.seconds) };
		if (d.type === 'wait_for') {
			return { type: d.type, condition: d.condition, seconds: Number(d.seconds) };
		}
		return { type: d.type };
	}

	function stepValid(d: StepDraft): boolean {
		if (d.type === 'command') return d.command.trim() !== '';
		if (d.type === 'wait' || d.type === 'wait_for') {
			const n = Number(d.seconds);
			return Number.isInteger(n) && n >= 1 && n <= maxWaitSeconds;
		}
		return true;
	}

	const steps = $derived(draft.map(toStep));
	const stepsOk = $derived(!isChain || (draft.length > 0 && draft.every(stepValid)));

	function addStep() {
		const first = offeredSteps[0]?.type ?? 'wait';
		draft.push({ id: nextStepId++, type: first, command: '', seconds: '60', condition: 'ready' });
	}

	function moveStep(i: number, by: -1 | 1) {
		const j = i + by;
		if (j < 0 || j >= draft.length) return;
		[draft[i], draft[j]] = [draft[j], draft[i]];
	}

	const playerAware = $derived(
		isChain
			? stopsServer(steps)
			: (scheduleKinds.find((k) => k.kind === effectiveKind)?.playerAware ?? false)
	);

	/**
	 * The fields an edit changes. One left as it was is not sent: a resent time or zone would
	 * reschedule the run and end a wait for players. A schedule that no longer stops the server
	 * stops waiting for players, which the daemon requires.
	 */
	const patchBody = $derived.by((): PatchSchedule => {
		if (!editBase) return {};
		const body: PatchSchedule = {};
		const baseBuilt = buildCron({ ...defaultCronForm(), ...readCron(editBase.cron) });
		if (built !== baseBuilt || moveZone) body.cron = built;
		if (moveZone) body.timezone = viewer;
		if (isChain && JSON.stringify(steps) !== baseSteps) body.steps = steps;
		if (isUpdateTest && applyTested !== (editBase.apply ?? false)) body.apply = applyTested;
		if (!playerAware) {
			if (editBase.wait_for_empty) body.wait_for_empty = false;
			return body;
		}
		if (waitForEmpty !== editBase.wait_for_empty) body.wait_for_empty = waitForEmpty;
		if (waitForEmpty && Number(maxDeferral) !== editBase.max_deferral_seconds)
			body.max_deferral_seconds = Number(maxDeferral);
		if (waitForEmpty && unknownPlayers !== editBase.unknown_players)
			body.unknown_players = unknownPlayers;
		return body;
	});
	const edited = $derived(Object.keys(patchBody).length > 0);
	/** Whether the form holds work that choosing another schedule to edit would lose. */
	const formDirty = $derived(editBase ? edited : draft.length > 0);
	unsaved(() => formDirty);

	const ready = $derived(
		(editBase ? edited : kind !== '') && built !== '' && stepsOk && !!zone && !saving
	);

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

	// Someone who may schedule nothing sees no list, so nothing is read or polled for them.
	const canSchedule = $derived(offered.length > 0);

	$effect(() => {
		if (canSchedule) void load();
	});

	$effect(() => {
		if (!canSchedule) return;
		const off = socket.subscribe(topics.state(instance.id), (m) => {
			if (m.type === 'maintenance') void load();
		});
		const unhook = socket.onConnected(() => void load());
		// A run that fires or is skipped sends no message, so upcoming times are re-read each
		// minute while the tab is visible, and when it becomes visible again.
		const visible = () => {
			if (!document.hidden) void load();
		};
		const tick = setInterval(visible, 60_000);
		document.addEventListener('visibilitychange', visible);
		return () => {
			off();
			unhook();
			clearInterval(tick);
			document.removeEventListener('visibilitychange', visible);
		};
	});

	async function load() {
		try {
			const page = await schedules.list();
			list = page.items;
			zone = page.timezone;
			const base = editBase;
			if (base && !list.some((s) => s.id === base.id)) stopEditing();
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
		started = null;
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
				...(isChain ? { steps } : {}),
				...(isUpdateTest ? { apply: applyTested } : {}),
				...(playerAware
					? {
							wait_for_empty: waitForEmpty,
							max_deferral_seconds: Number(maxDeferral),
							unknown_players: unknownPlayers
						}
					: {})
			});
			kind = '';
			draft = [];
			applyTested = false;
		});
	}

	/** Fills the form with s's time, steps and waiting rules and moves focus to it. */
	async function edit(s: Schedule) {
		if (formDirty) {
			switching = s;
			discardOpen = true;
			return;
		}
		editBase = s;
		moveZone = false;
		form = { ...defaultCronForm(), ...readCron(s.cron) };
		draft = (s.steps ?? []).map(toDraft);
		baseSteps = JSON.stringify(draft.map(toStep));
		waitForEmpty = s.wait_for_empty;
		maxDeferral = String(s.max_deferral_seconds);
		unknownPlayers = s.unknown_players;
		applyTested = s.apply ?? false;
		await tick();
		formHeading?.focus();
	}

	/** Leaves an edit and returns the form to a new schedule's starting values. */
	function stopEditing() {
		editBase = null;
		moveZone = false;
		kind = '';
		draft = [];
		form = defaultCronForm();
		waitForEmpty = false;
		maxDeferral = '7200';
		unknownPlayers = 'wait';
		applyTested = false;
	}

	/** Leaves an edit the user ended and moves focus back to the form's heading. */
	function finishEditing() {
		stopEditing();
		formHeading?.focus();
	}

	/** Writes the fields the form changed over the schedule being edited. */
	function save() {
		const target = editBase;
		if (!target) return;
		const body = patchBody;
		void act(async () => {
			await schedules.patch(target.id, body);
			finishEditing();
		});
	}

	/** This server's schedules that run in another zone than the viewer's and that they may edit. */
	const movable = $derived(mine.filter((s) => s.timezone !== viewer && canEdit(s)));
	let moveOpen = $state(false);

	/** What moving s to the viewer's zone does, in a line: its words or cron, and both zones. */
	function describeMove(s: Schedule): string {
		return `${label(s.kind)}, ${describeExpression(s.cron) || s.cron} in ${s.timezone}`;
	}

	/** Moves each of the movable schedules to the viewer's zone, keeping its clock times as
	 * written, as the switch on a single schedule does. */
	function moveAll() {
		const targets = movable;
		void act(async () => {
			try {
				for (const s of targets) {
					const cron = buildCron({ ...defaultCronForm(), ...readCron(s.cron) });
					await schedules.patch(s.id, { cron, timezone: viewer });
				}
			} finally {
				await load();
			}
		});
	}

	const toggle = (s: Schedule, enabled: boolean) =>
		void act(() => schedules.patch(s.id, { enabled }));

	function remove() {
		const target = deleting;
		deleteOpen = false;
		if (target?.id === editBase?.id) stopEditing();
		if (target) void act(() => schedules.remove(target.id));
	}

	/** The schedule chosen for editing while the form held unsaved work. */
	let switching = $state<Schedule | null>(null);
	let discardOpen = $state(false);

	/** Drops the form's unsaved work and edits the schedule that was waiting for it. */
	function discardAndEdit() {
		const next = switching;
		switching = null;
		stopEditing();
		if (next) void edit(next);
	}

	function ask(s: Schedule) {
		deleting = s;
		deleteOpen = true;
	}

	/** The schedule whose run now is being confirmed, and the label of the last one started. */
	let runTarget = $state<Schedule | null>(null);
	let runOpen = $state(false);
	let started = $state<string | null>(null);

	/** What running s now does, for its confirmation, as saved rather than as the form holds it. */
	function runDescription(s: Schedule): string {
		const what = s.steps ? `${label(s.kind)}: ${describeSteps(s.steps)}.` : `${label(s.kind)}.`;
		const stops = s.steps
			? stopsServer(s.steps)
			: (scheduleKinds.find((k) => k.kind === s.kind)?.playerAware ?? false);
		return [
			what,
			stops && 'Connected players are disconnected.',
			`It runs now${s.wait_for_empty ? ' without waiting for players to leave' : ''}, and its next run stays as it is.`,
			formDirty && editBase?.id === s.id && 'It runs as saved, without the changes in the form.'
		]
			.filter(Boolean)
			.join(' ');
	}

	function askRun(s: Schedule) {
		runTarget = s;
		runOpen = true;
	}

	/** Starts the confirmed schedule's run now. */
	function runNow() {
		const target = runTarget;
		if (!target) return;
		void act(async () => {
			await schedules.run(target.id);
			started = label(target.kind);
		});
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
			Run backups, restarts, game updates, update tests on a copy and sequences of steps
			automatically at scheduled times. If the server is busy when a task is due, that run is
			skipped.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		{#if offered.length === 0}
			<p class="text-sm text-muted-foreground" data-testid="schedules-blocked">
				Scheduling is not available to you.
			</p>
		{:else}
			<Problem error={failure} />
			<p class={['text-sm text-muted-foreground', !started && 'sr-only']} role="status">
				{#if started}
					Started “{started}”. Its progress shows under
					<a class="underline" href={resolve('/instances/[id]', { id: instance.id })}>Operations</a> on
					the overview.
				{/if}
			</p>

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
				{#if movable.length > 0}
					<div
						class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-dashed p-3"
					>
						<p class="text-sm text-muted-foreground">
							{movable.length === 1 ? '1 schedule runs' : `${movable.length} schedules run`} in another
							time zone than yours ({viewer}).
						</p>
						<Button
							variant="outline"
							size="sm"
							disabled={saving || editBase !== null}
							onclick={() => (moveOpen = true)}
						>
							Move to {viewer}
						</Button>
					</div>
				{/if}
				<div class="grid gap-2">
					{#each mine as s (s.id)}
						{@const words = describeExpression(s.cron)}
						<div class="flex flex-wrap items-center gap-3 rounded-lg border p-3">
							<div class="grid min-w-0 grow basis-64 gap-1">
								<div class="flex flex-wrap items-center gap-2">
									<span class="text-sm font-medium">{label(s.kind)}</span>
									{#if s.enabled}
										<span class="text-sm">next {when(s.next_run_at)}</span>
									{/if}
									{#if !s.enabled}<Badge variant="secondary">paused</Badge>{/if}
									{#if s.deferred_since}<Badge>waiting for players</Badge>{/if}
								</div>
								{#if words}
									<p class="text-sm" data-testid="schedule-words">
										{words}{s.timezone !== viewer ? ` (${s.timezone})` : ''}
									</p>
								{/if}
								{#if s.steps}
									<p class="text-sm" data-testid="chain-steps">{describeSteps(s.steps)}</p>
								{/if}
								<!-- Times are in the viewer's zone; the cron is written in the schedule's own. -->
								<p class="text-sm text-muted-foreground">
									last {when(s.last_run_at)} · <span class="font-mono">{s.cron}</span>
									{#if s.timezone !== viewer}in {s.timezone}, times shown in {viewer}{/if}
									{#if s.created_by_username}· set up by {s.created_by_username}{/if}
									{#if s.wait_for_empty}· waits up to {deferral(s.max_deferral_seconds)} for players to
										leave{/if}
									{#if s.apply}· applies the updates when every mod loads and nobody is on{/if}
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
							<div class="flex items-center gap-1">
								<Switch
									checked={s.enabled}
									disabled={saving}
									onCheckedChange={(v) => toggle(s, v)}
									aria-label="Enable {label(s.kind)} schedule"
								/>
								{#if canEdit(s) && !s.deferred_since}
									<Button variant="ghost" size="sm" disabled={saving} onclick={() => askRun(s)}>
										<Play /> Run now<span class="sr-only">: {label(s.kind)}</span>
									</Button>
								{/if}
								{#if canEdit(s)}
									<Button variant="ghost" size="sm" disabled={saving} onclick={() => edit(s)}>
										<Pencil />
										<span class="sr-only">Edit the {label(s.kind)} schedule</span>
									</Button>
								{/if}
								<Button variant="ghost" size="sm" disabled={saving} onclick={() => ask(s)}>
									<Trash2 />
									<span class="sr-only">Delete schedule</span>
								</Button>
							</div>
						</div>
					{/each}
				</div>
			{/if}

			<div class="grid gap-3 border-t pt-4">
				<h3 class="text-sm font-medium outline-none" tabindex="-1" bind:this={formHeading}>
					{editBase ? `Edit “${label(editBase.kind)}”` : 'Add a schedule'}
				</h3>
				<div class={['grid gap-2', editBase && 'hidden']}>
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
				{#if isChain}
					<div class="grid gap-2" data-testid="chain-editor">
						<Label>Steps, in order</Label>
						{#each draft as d, i (d.id)}
							<!-- Number, the step's own fields, and a fixed icon group, so every row's controls
							     line up whatever the step asks for. -->
							<div class="grid grid-cols-[1.25rem_minmax(0,1fr)_auto] items-start gap-2">
								<span class="pt-2 text-sm text-muted-foreground">{i + 1}.</span>
								<div class="flex min-w-0 flex-wrap items-center gap-2">
									<Select.Root type="single" bind:value={d.type}>
										<Select.Trigger class="w-56 max-w-full" aria-label="Step {i + 1} type">
											{chainSteps.find((c) => c.type === d.type)?.label}
										</Select.Trigger>
										<Select.Content>
											{#each offeredSteps as option (option.type)}
												<Select.Item value={option.type}>{option.label}</Select.Item>
											{/each}
										</Select.Content>
									</Select.Root>
									{#if d.type === 'command'}
										<Input
											bind:value={d.command}
											class="min-w-48 flex-1 font-mono"
											autocomplete="off"
											placeholder="say Restarting in 5 minutes"
											aria-label="Step {i + 1} command"
										/>
									{:else if d.type === 'wait'}
										<Input
											inputmode="numeric"
											autocomplete="off"
											bind:value={d.seconds}
											class="w-28"
											aria-label="Step {i + 1} seconds (1 to {maxWaitSeconds})"
										/>
										<span class="text-sm text-muted-foreground">seconds</span>
									{:else if d.type === 'wait_for'}
										<Select.Root type="single" bind:value={d.condition}>
											<Select.Trigger class="w-48" aria-label="Step {i + 1} condition">
												{waitConditions.find((c) => c.value === d.condition)?.label}
											</Select.Trigger>
											<Select.Content>
												{#each waitConditions as option (option.value)}
													<Select.Item value={option.value}>{option.label}</Select.Item>
												{/each}
											</Select.Content>
										</Select.Root>
										<span class="text-sm text-muted-foreground">for at most</span>
										<Input
											inputmode="numeric"
											autocomplete="off"
											bind:value={d.seconds}
											class="w-28"
											aria-label="Step {i + 1} longest wait in seconds (1 to {maxWaitSeconds})"
										/>
										<span class="text-sm text-muted-foreground">seconds</span>
									{/if}
								</div>
								<div class="flex items-center">
									<Button
										variant="ghost"
										size="sm"
										disabled={i === 0}
										onclick={() => moveStep(i, -1)}
									>
										<ArrowUp />
										<span class="sr-only">Move step {i + 1} up</span>
									</Button>
									<Button
										variant="ghost"
										size="sm"
										disabled={i === draft.length - 1}
										onclick={() => moveStep(i, 1)}
									>
										<ArrowDown />
										<span class="sr-only">Move step {i + 1} down</span>
									</Button>
									<Button variant="ghost" size="sm" onclick={() => draft.splice(i, 1)}>
										<Trash2 />
										<span class="sr-only">Remove step {i + 1}</span>
									</Button>
								</div>
							</div>
						{/each}
						<Button
							size="sm"
							variant="outline"
							class="justify-self-start"
							disabled={draft.length >= maxChainSteps}
							onclick={addStep}
						>
							<Plus /> Add step
						</Button>
						<p class="text-sm text-muted-foreground">
							Steps run one after another and the sequence stops at the first that fails. A command
							is skipped when the server is not running or has no RCON mod. Stopping a stopped
							server, or starting a running one, counts as done. A wait for the server ends when its
							condition holds or after its longest wait, whichever comes first, and the sequence
							goes on either way.
						</p>
					</div>
				{/if}
				<div class="grid gap-2">
					<Label for="schedule-every">How often</Label>
					<div class="flex flex-wrap items-end gap-2">
						<Select.Root type="single" bind:value={form.every}>
							<Select.Trigger id="schedule-every" class="w-48"
								>{everyLabels[form.every]}</Select.Trigger
							>
							<Select.Content>
								{#each Object.entries(everyLabels) as [value, text] (value)}
									<Select.Item {value}>{text}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>

						{#if form.every === 'hours'}
							<Select.Root type="single" bind:value={form.everyHours}>
								<Select.Trigger class="w-32" aria-label="Hours between runs">
									{form.everyHours} hours
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
								bind:value={form.startAt}
								aria-label="Starting at"
								step="60"
							/>
						{/if}

						{#if form.every === 'month'}
							<Select.Root type="single" bind:value={form.monthDay}>
								<Select.Trigger class="w-36" aria-label="Day of the month">
									on the {ordinal(Number(form.monthDay))}
								</Select.Trigger>
								<Select.Content>
									{#each monthDays as d (d)}
										<Select.Item value={d}>{ordinal(Number(d))}</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						{/if}

						{#if form.every === 'day' || form.every === 'days' || form.every === 'month'}
							<!-- Native time input: a locale-correct picker with no library behind it. -->
							<Input
								type="time"
								class="w-32"
								bind:value={form.atTime}
								aria-label="Time of day"
								step="60"
							/>
						{/if}
					</div>
					{#if form.every === 'days'}
						<div class="flex flex-wrap gap-1" role="group" aria-label="Days of the week">
							{#each weekdayChoices as d (d.value)}
								{@const on = form.weekdays.includes(d.value)}
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

				{#if form.every === 'custom'}
					<div class="grid gap-2">
						<Label for="schedule-cron">Cron expression</Label>
						<!--
							The expression is the daemon's to validate: it answers an invalid one with the
							field and its own help text, so there is no second, weaker parser here.
						-->
						<Input
							id="schedule-cron"
							bind:value={form.custom}
							autocomplete="off"
							placeholder="0 4 * * *"
							class="font-mono"
						/>
						<p class="text-sm text-muted-foreground">
							Five fields: minute, hour, day of month, month, day of week. Shorthands such as
							<span class="font-mono">@daily</span> work too. Once {editBase ? 'saved' : 'added'},
							its next runs appear under Upcoming runs.
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
						<p class={['text-sm', warningText]} data-testid="clock-change">
							{runZone} changes its clocks at this time. {clashNote} Choose another time to avoid it.
						</p>
					{/if}
				{/if}

				<p class="text-sm text-muted-foreground">
					{#if zone}
						{#if editBase}
							The times above are in {runZone}, where this schedule runs.
						{:else}
							New schedules run in {viewer}. Existing schedules keep their own zone.
						{/if}
						Times on this page are shown in {viewer}.

						<a class="underline" href={resolve('account/date-time')}>Change time zone</a>
					{:else}
						Scheduler timezone is unavailable. Reload this page before creating a schedule.
					{/if}
				</p>

				{#if editBase && editBase.timezone !== viewer}
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="schedule-move-zone">Run it in {viewer} instead</Label>
							<p id="schedule-move-zone-help" class="text-sm text-muted-foreground">
								The times above stay as they are and are read in {viewer}.
							</p>
						</div>
						<Switch
							id="schedule-move-zone"
							aria-describedby="schedule-move-zone-help"
							bind:checked={moveZone}
						/>
					</div>
				{/if}

				{#if isUpdateTest}
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="schedule-apply">Apply when every mod loads and nobody is on</Label>
							<p id="schedule-apply-help" class="text-sm text-muted-foreground">
								Otherwise the copy is kept for you to apply or discard. Either way, an Update test
								finished rule tells you the result.
							</p>
						</div>
						<Switch
							id="schedule-apply"
							aria-describedby="schedule-apply-help"
							bind:checked={applyTested}
						/>
					</div>
				{/if}

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
							minutes after the server starts. With the RCON mod installed, players are warned in
							chat when the run starts waiting and 5 minutes before the latest time.
						</p>
					{/if}
				{/if}

				{#if editBase}
					<div class="flex flex-wrap gap-2">
						<Button size="sm" disabled={!ready} onclick={save}>Save changes</Button>
						<Button size="sm" variant="outline" disabled={saving} onclick={finishEditing}
							>Cancel</Button
						>
					</div>
				{:else}
					<Button size="sm" class="justify-self-start" disabled={!ready} onclick={create}>
						Add schedule
					</Button>
				{/if}
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
				<span class="font-mono">({deleting?.cron})</span> stops running on its own. Runs already listed
				under Operations are kept.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (deleteOpen = false)}>Cancel</Button>

			<Button variant="destructive" onclick={remove}>Delete schedule</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<DestructiveConfirm
	bind:open={discardOpen}
	title="Discard the changes in the form?"
	description="The form holds changes that are not saved. Editing {switching
		? `“${label(switching.kind)}”`
		: 'another schedule'} replaces them."
	confirmLabel="Discard changes"
	onconfirm={discardAndEdit}
/>

<DestructiveConfirm
	bind:open={moveOpen}
	title="Move {movable.length === 1
		? 'this schedule'
		: `these ${movable.length} schedules`} to {viewer}?"
	description="Each keeps its clock times and reads them in {viewer} from now on, so it runs at a different moment than today. {movable
		.map(describeMove)
		.join('; ')}."
	confirmLabel="Move to {viewer}"
	onconfirm={moveAll}
/>

<DestructiveConfirm
	bind:open={runOpen}
	title="Run this schedule now?"
	description={runTarget ? runDescription(runTarget) : ''}
	confirmLabel="Run now"
	onconfirm={runNow}
/>
