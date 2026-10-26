<script lang="ts">
	import type {
		AlertRule,
		CreateAlertRule,
		NotificationsApi,
		RuleKind,
		Webhook
	} from '#lib/api/notifications.js';
	import { viewerZone } from '#lib/api/schedules.js';
	import {
		DEFAULT_STYLE,
		MAX_TEMPLATE,
		expand,
		styleOf,
		styleParams,
		type MessageStyle,
		type NotifyEvent
	} from '#lib/message.js';
	import {
		DEFAULT_CRASH_COUNT,
		DEFAULT_CRASH_WINDOW_MINUTES,
		DEFAULT_STALE_FACTOR,
		DEFAULT_STUCK_MINUTES,
		GROUPS,
		HOST_WIDE,
		NOTIFY_EVENTS,
		TIMED,
		clock,
		eventOf,
		isCondition,
		minutesOf,
		withoutFields
	} from '#lib/notification-events.js';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import DiscordPreview from '#lib/components/discord-preview.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import MessageStyleFields from '#lib/components/message-style-fields.svelte';
	import MessageTemplateField from '#lib/components/message-template-field.svelte';
	import Problem from '#lib/components/problem.svelte';
	import QuietHoursFields from '#lib/components/quiet-hours-fields.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Send from '@lucide/svelte/icons/send';

	interface Quiet {
		on: boolean;
		start: string;
		end: string;
		zone: string;
	}

	/** A rule as the form holds it. Thresholds as typed: null is an empty field, the default. */
	interface Draft {
		kind: RuleKind | null;
		channels: string[];
		template: string;
		style: MessageStyle;
		quiet: Quiet;
		crashCount: number | null;
		crashWindowMinutes: number | null;
		stuckMinutes: number | null;
		staleFactor: number | null;
		timezone: string;
	}

	/**
	 * Adds, edits and duplicates an alert rule in a dialog: first the event, then where it posts,
	 * what it says and when it stays quiet. Open it with `add`, `edit` or `duplicate`. `channels`
	 * are the webhooks a rule in this scope may post to, and `server` names the server in the
	 * preview, '' for a panel-wide rule.
	 */
	let {
		api,
		channels,
		server,
		hostWide = false,
		pluginMissing = false,
		onsaved
	}: {
		api: NotificationsApi;
		channels: Webhook[];
		server: string;
		/** Whether host-wide events, such as low disk, are offered. */
		hostWide?: boolean;
		/** Whether the server lacks the ValminEye plugin that game events come from. */
		pluginMissing?: boolean;
		onsaved: () => void;
	} = $props();

	const zones =
		typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [];
	const noQuiet = (): Quiet => ({ on: false, start: '22:00', end: '07:00', zone: viewerZone() });

	let open = $state(false);
	let editing = $state<AlertRule | null>(null);
	let draft = $state<Draft>(blank());
	let baseline = $state('');
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let tests = $state<{ id: string; name: string; job: string }[]>([]);

	const event = $derived(draft.kind ? eventOf(draft.kind) : null);
	const offered = $derived(NOTIFY_EVENTS.filter((e) => hostWide || !HOST_WIDE.includes(e.kind)));
	const dirty = $derived(open && JSON.stringify(draft) !== baseline);
	const problem = $derived(event ? problemOf(event) : '');
	const previewServer = $derived(
		event && HOST_WIDE.includes(event.kind) ? '' : server || 'Your server'
	);

	unsaved(() => dirty);

	/** Opens the dialog on the event list, for a new rule. */
	export function add() {
		start(null, null);
	}

	/** Opens the dialog on rule's settings, to change it. */
	export function edit(rule: AlertRule) {
		start(rule, rule);
	}

	/** Opens the dialog on a copy of rule's settings, to save as a new rule. */
	export function duplicate(rule: AlertRule) {
		start(null, rule);
	}

	function start(target: AlertRule | null, source: AlertRule | null) {
		editing = target;
		draft = source ? fromRule(source) : blank();
		baseline = JSON.stringify(draft);
		failure = null;
		tests = [];
		open = true;
	}

	function blank(): Draft {
		return {
			kind: null,
			channels: channels?.length === 1 ? [channels[0].id] : [],
			template: '',
			style: DEFAULT_STYLE,
			quiet: noQuiet(),
			crashCount: null,
			crashWindowMinutes: null,
			stuckMinutes: null,
			staleFactor: null,
			timezone: ''
		};
	}

	/** A stored rule as the form holds it. A rule that hid fields before templates could gets the
	 * default template without their lines. */
	function fromRule(rule: AlertRule): Draft {
		const p = rule.params;
		const minutes = (seconds?: number) => (seconds === undefined ? null : seconds / 60);
		const { quiet_start_minutes: from, quiet_end_minutes: until, quiet_timezone: zone } = rule;
		return {
			kind: rule.condition_kind,
			channels: [...rule.webhook_ids],
			template:
				p.template || withoutFields(eventOf(rule.condition_kind).template, p.hidden_fields ?? []),
			style: styleOf(p),
			quiet:
				from === null || until === null || !zone
					? noQuiet()
					: { on: true, start: clock(from), end: clock(until), zone },
			crashCount: p.crash_count ?? null,
			crashWindowMinutes: minutes(p.crash_window_seconds),
			stuckMinutes: minutes(p.stuck_after_seconds),
			staleFactor: p.stale_factor ?? null,
			timezone: p.timezone ?? ''
		};
	}

	function choose(kind: RuleKind) {
		draft.kind = kind;
		draft.template = eventOf(kind).template;
	}

	/** The rule as it is sent. The default message is stored as no template, so a better default
	 * reaches the rule too. */
	function bodyOf(e: NotifyEvent): CreateAlertRule {
		const d = draft;
		const seconds = (minutes: number | null) =>
			minutes === null ? undefined : Math.round(minutes * 60);
		const params: AlertRule['params'] = {};
		if (e.kind === 'crash_loop') {
			if (d.crashCount !== null) params.crash_count = d.crashCount;
			if (d.crashWindowMinutes !== null)
				params.crash_window_seconds = seconds(d.crashWindowMinutes);
		} else if (e.kind === 'job_stuck') {
			if (d.stuckMinutes !== null) params.stuck_after_seconds = seconds(d.stuckMinutes);
		} else if (e.kind === 'stale_backup') {
			if (d.staleFactor !== null) params.stale_factor = d.staleFactor;
		}
		if (TIMED.includes(e.kind) && d.timezone.trim()) params.timezone = d.timezone.trim();
		const template = d.template.trim();
		if (template && template !== e.template) params.template = template;
		return {
			condition_kind: e.kind,
			webhook_ids: d.channels,
			params: { ...params, ...styleParams(d.style) },
			quiet_start_minutes: d.quiet.on ? minutesOf(d.quiet.start) : 0,
			quiet_end_minutes: d.quiet.on ? minutesOf(d.quiet.end) : 0,
			quiet_timezone: d.quiet.on ? d.quiet.zone.trim() : ''
		};
	}

	// Mirrors the daemon: counts and durations are 0 or more, a stale factor 0 or above 1, and a
	// window whose start equals its end is empty.
	const badCount = (n: number | null) => n !== null && !(Number.isInteger(n) && n >= 0);
	const badMinutes = (m: number | null) => m !== null && !(m >= 0);
	const badFactor = (f: number | null) => f !== null && f !== 0 && !(f > 1);
	const badQuiet = (q: Quiet) =>
		q.on && (q.start === '' || q.end === '' || q.zone.trim() === '' || q.start === q.end);

	/** Why the rule cannot be saved yet, or ''. */
	function problemOf(e: NotifyEvent): string {
		const d = draft;
		if (d.channels.length === 0) return 'Choose at least one channel to post to.';
		if (d.template.length > MAX_TEMPLATE)
			return `The message can be at most ${MAX_TEMPLATE} characters.`;
		if (badQuiet(d.quiet)) return 'Quiet hours need a start, a different end and a timezone.';
		if (
			(e.kind === 'crash_loop' && (badCount(d.crashCount) || badMinutes(d.crashWindowMinutes))) ||
			(e.kind === 'job_stuck' && badMinutes(d.stuckMinutes)) ||
			(e.kind === 'stale_backup' && badFactor(d.staleFactor))
		)
			return 'Check the numbers.';
		return '';
	}

	async function save() {
		if (!event || problem) return;
		saving = true;
		failure = null;
		try {
			const body = bodyOf(event);
			if (editing) await api.rules.update(editing.id, body);
			else await api.rules.create(body);
			open = false;
			onsaved();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	/** Posts the message this rule would send, over the preview's sample values, to each chosen
	 * channel. */
	async function sendTest() {
		if (!event) return;
		const e = event;
		const params = bodyOf(e).params ?? {};
		failure = null;
		try {
			tests = await Promise.all(
				draft.channels.map(async (id) => ({
					id,
					name: channels.find((c) => c.id === id)?.name ?? id,
					job: (
						await api.rules.test({
							condition_kind: e.kind,
							params,
							webhook_id: id,
							fields: e.sample
						})
					).job_id
				}))
			);
		} catch (err) {
			failure = err;
		}
	}

	const toggled = (list: string[], item: string, on: boolean) =>
		on ? [...list, item] : list.filter((other) => other !== item);
	// Unsaved changes survive a stray click outside the dialog or Escape; Cancel discards them.
	const keepOpen = (ev: Event) => {
		if (dirty) ev.preventDefault();
	};
</script>

<Dialog.Root bind:open>
	<Dialog.Content
		class="max-h-[90vh] overflow-y-auto sm:max-w-3xl"
		onInteractOutside={keepOpen}
		onEscapeKeydown={keepOpen}
	>
		{#if !event}
			<Dialog.Header>
				<Dialog.Title>Add rule</Dialog.Title>
				<Dialog.Description>Choose what should be posted.</Dialog.Description>
			</Dialog.Header>
			<div class="grid gap-5">
				{#each GROUPS as group (group.name)}
					{@const events = offered.filter((e) => e.group === group.name)}
					{#if events.length > 0}
						<section class="grid gap-2" aria-labelledby="pick-{group.name}">
							<div class="grid gap-0.5">
								<h3 id="pick-{group.name}" class="text-sm font-medium">{group.name}</h3>
								<p class="text-xs text-muted-foreground">
									{group.note}
									{#if group.game && pluginMissing}
										These need the ValminEye plugin on the server.
									{/if}
								</p>
							</div>
							<div class="grid gap-2 sm:grid-cols-2">
								{#each events as e (e.kind)}
									<button
										type="button"
										class="grid gap-0.5 rounded-lg border p-3 text-left hover:bg-muted/50 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
										data-testid="pick-{e.kind}"
										onclick={() => choose(e.kind)}
									>
										<span class="text-sm font-medium">{e.label}</span>
										<span class="text-xs text-muted-foreground">{e.when}</span>
									</button>
								{/each}
							</div>
						</section>
					{/if}
				{/each}
			</div>
		{:else}
			<Dialog.Header>
				<Dialog.Title>{editing ? 'Edit rule' : 'Add rule'} · {event.label}</Dialog.Title>
				<Dialog.Description>{event.when}</Dialog.Description>
			</Dialog.Header>

			<div class="grid gap-5">
				<div class="grid gap-2">
					<h3 class="text-sm font-medium">Post to</h3>
					{#if channels.length === 0}
						<p class="text-sm text-muted-foreground">
							No channels yet. Add one under Channels first.
						</p>
					{:else}
						<div class="flex flex-wrap gap-2">
							{#each channels as c (c.id)}
								<label
									class="flex cursor-pointer items-center gap-2 rounded-md border px-3 py-1.5 text-sm has-[:checked]:border-primary has-[:checked]:bg-primary/5 has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring"
								>
									<input
										type="checkbox"
										class="accent-primary"
										checked={draft.channels.includes(c.id)}
										onchange={(ev) =>
											(draft.channels = toggled(draft.channels, c.id, ev.currentTarget.checked))}
									/>
									{c.name}
									{#if server && c.instance_id === null}<Badge variant="secondary">shared</Badge
										>{/if}
									{#if !c.enabled}<Badge variant="outline">paused</Badge>{/if}
								</label>
							{/each}
						</div>
					{/if}
				</div>

				{#if event.kind === 'crash_loop'}
					<div class="grid gap-3 sm:grid-cols-2">
						<div class="grid gap-2">
							<Label for="rule-crash-count">Stops</Label>
							<Input
								id="rule-crash-count"
								type="number"
								min="0"
								step="1"
								placeholder={String(DEFAULT_CRASH_COUNT)}
								aria-invalid={badCount(draft.crashCount)}
								bind:value={draft.crashCount}
							/>
						</div>
						<div class="grid gap-2">
							<Label for="rule-crash-window">Within minutes</Label>
							<Input
								id="rule-crash-window"
								type="number"
								min="0"
								placeholder={String(DEFAULT_CRASH_WINDOW_MINUTES)}
								aria-invalid={badMinutes(draft.crashWindowMinutes)}
								bind:value={draft.crashWindowMinutes}
							/>
						</div>
					</div>
				{:else if event.kind === 'job_stuck'}
					<div class="grid gap-2 sm:w-1/2">
						<Label for="rule-stuck-minutes">Minutes a job may run</Label>
						<Input
							id="rule-stuck-minutes"
							type="number"
							min="0"
							placeholder={String(DEFAULT_STUCK_MINUTES)}
							aria-invalid={badMinutes(draft.stuckMinutes)}
							bind:value={draft.stuckMinutes}
						/>
					</div>
				{:else if event.kind === 'stale_backup'}
					<div class="grid gap-2 sm:w-1/2">
						<Label for="rule-stale-factor">Times the backup interval</Label>
						<Input
							id="rule-stale-factor"
							type="number"
							step="0.1"
							placeholder={String(DEFAULT_STALE_FACTOR)}
							aria-invalid={badFactor(draft.staleFactor)}
							bind:value={draft.staleFactor}
						/>
					</div>
				{/if}

				<div class="grid gap-4 md:grid-cols-2">
					<MessageTemplateField
						id="rule-message"
						bind:value={draft.template}
						fallback={event.template}
						placeholders={event.placeholders}
					/>
					<div class="grid content-start gap-2">
						<DiscordPreview
							data-testid="rule-preview"
							server={previewServer}
							headline={event.headline}
							text={expand(draft.template.trim() || event.template, {
								...event.sample,
								server: previewServer,
								event: event.headline
							})}
							style={draft.style}
							accent={event.accent}
						/>
						{#if isCondition(event.kind)}
							<p class="text-xs text-muted-foreground">
								When it clears, this message is edited to show it’s resolved.
							</p>
						{/if}
					</div>
				</div>

				{#if TIMED.includes(event.kind)}
					<div class="grid gap-2 sm:w-1/2">
						<Label for="rule-zone">Timezone for times in the message</Label>
						<Input id="rule-zone" list="rule-zones" placeholder="UTC" bind:value={draft.timezone} />
					</div>
				{/if}

				<div class="grid gap-2">
					<h3 class="text-sm font-medium">Message style</h3>
					<MessageStyleFields id="rule-style" bind:style={draft.style} accent={event.accent} />
				</div>

				<div class="grid gap-3">
					<QuietHoursFields
						id="rule-quiet"
						zonesList="rule-zones"
						bind:on={draft.quiet.on}
						bind:start={draft.quiet.start}
						bind:end={draft.quiet.end}
						bind:zone={draft.quiet.zone}
						invalid={badQuiet(draft.quiet)}
						hint="Alerts still open when quiet hours end are sent then; one that opens and clears inside the window is not sent."
					/>
				</div>
				<datalist id="rule-zones">
					{#each zones as zone (zone)}<option value={zone}></option>{/each}
				</datalist>

				{#each tests as t (t.id)}
					<div class="grid gap-1 rounded-lg border p-3">
						<p class="text-sm font-medium">Test to {t.name}</p>
						<JobProgress jobId={t.job} />
					</div>
				{/each}

				<Problem error={failure} />
			</div>

			<Dialog.Footer class="flex-wrap items-center gap-2 sm:justify-between">
				<p class="text-sm text-destructive sm:mr-auto" role="status">{problem}</p>
				<div class="flex flex-wrap gap-2">
					{#if !editing}
						<Button variant="ghost" onclick={() => (draft.kind = null)}>
							<ArrowLeft /> Back
						</Button>
					{/if}
					<Button
						variant="outline"
						disabled={draft.channels.length === 0 || problem !== ''}
						onclick={sendTest}
					>
						<Send /> Send test
					</Button>
					<Button variant="outline" onclick={() => (open = false)}>Cancel</Button>
					<Button disabled={saving || problem !== ''} onclick={save}>
						{saving ? 'Saving…' : editing ? 'Save rule' : 'Add rule'}
					</Button>
				</div>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
