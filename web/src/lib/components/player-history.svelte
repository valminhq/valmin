<script lang="ts">
	import { AreaChart } from 'layerchart';
	import { instances } from '$lib/api/instances';
	import { playerHistory, type PlayerHistoryRange, type PlayerObservation } from '$lib/api/players';
	import { playerActivity, type PlayerInterval } from '$lib/player-activity';
	import { socket, socketStatus } from '$lib/socket/index.svelte';
	import { topics, type ServerMessage } from '$lib/socket/messages';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import Problem from '$lib/components/problem.svelte';

	let { instanceId }: { instanceId: string } = $props();
	const DAY = 86_400_000;
	let days = $state(1);
	let fixedEnd = $state<number | null>(null);
	let history = $state<{ range: PlayerHistoryRange; points: PlayerObservation[] } | null>(null);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let updatedAt = $state<number | null>(null);
	let nowReading = $state<{ players: number | null; at: number } | null>(null);
	let clock = $state(Date.now());
	let selected = $state(-1);
	let tablePage = $state(0);
	let retry = $state(0);
	let previousWindow = '';
	const activity = $derived(history ? playerActivity(history.range, history.points) : null);
	const intervals = $derived(activity?.intervals ?? []);
	const chosen = $derived(selected >= 0 ? intervals[selected] : undefined);
	const rangeStart = $derived(history ? Date.parse(history.range.from) : 0);
	const rangeEnd = $derived(history ? Date.parse(history.range.to) : 0);
	const duration = $derived(rangeEnd - rangeStart);
	const yMax = $derived(Math.max(1, activity?.peak ?? 0));
	const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
	const nowStale = $derived(
		socketStatus.value !== 'open' ||
			!nowReading ||
			!Number.isFinite(nowReading.at) ||
			clock - nowReading.at > 15_000
	);
	const chartSeries = $derived.by(() => {
		const groups: Array<{
			key: string;
			data: Array<{ t: number; v: number }>;
			color: string;
			props: { line: true };
		}> = [];
		let current: (typeof groups)[number] | null = null;
		for (const interval of intervals) {
			if (interval.players === null) {
				current = null;
				continue;
			}
			if (!current) {
				current = {
					key: String(groups.length),
					data: [],
					color: 'var(--chart-2)',
					props: { line: true }
				};
				groups.push(current);
			}
			current.data.push({ t: interval.from, v: interval.players });
			current.data.push({ t: interval.to, v: interval.players });
		}
		return groups;
	});
	const chartData = $derived(chartSeries.flatMap((group) => group.data));
	const xTicks = $derived(
		[0, 0.25, 0.5, 0.75, 1].map((part) => ({ part, at: rangeStart + part * duration }))
	);
	const yTicks = $derived([...new Set([0, Math.ceil(yMax / 2), yMax])]);
	const tableRows = $derived([...intervals].reverse().slice(tablePage * 25, (tablePage + 1) * 25));

	$effect(() => {
		const id = instanceId;
		const span = days * DAY;
		const end = fixedEnd;
		void retry;
		let disposed = false;
		let inFlight = false;
		const key = `${id}:${span}:${end}`;
		if (previousWindow !== key) history = null;
		previousWindow = key;
		failure = null;
		loading = true;
		selected = -1;
		tablePage = 0;
		async function reload() {
			if (disposed || inFlight || (end === null && document.visibilityState === 'hidden')) return;
			inFlight = true;
			const requestedTo = new Date(end ?? Date.now()).toISOString();
			const requestedFrom = new Date(Date.parse(requestedTo) - span).toISOString();
			try {
				const first = await playerHistory.range(id, requestedFrom, requestedTo);
				const points = [...first.items];
				let cursor = first.next_cursor;
				while (cursor) {
					const page = await playerHistory.range(id, first.range.from, first.range.to, cursor);
					points.push(...page.items);
					cursor = page.next_cursor;
				}
				if (disposed) return;
				history = { range: first.range, points };
				updatedAt = Date.now();
				failure = null;
				selected = -1;
				tablePage = 0;
			} catch (error) {
				if (!disposed) failure = error;
			} finally {
				if (!disposed) {
					inFlight = false;
					loading = false;
				}
			}
		}
		void reload();
		const refresh = setInterval(() => {
			if (end === null) void reload();
		}, 15_000);
		const connected = socket.onConnected(() => {
			if (end === null) void reload();
		});
		const visible = () => {
			if (document.visibilityState === 'visible' && end === null) void reload();
		};
		document.addEventListener('visibilitychange', visible);
		return () => {
			disposed = true;
			clearInterval(refresh);
			connected();
			document.removeEventListener('visibilitychange', visible);
		};
	});

	$effect(() => {
		const id = instanceId;
		let disposed = false;
		let received = false;
		nowReading = null;
		const off = socket.subscribe(topics.stats(id), (message: ServerMessage) => {
			if (message.type !== 'stats') return;
			received = true;
			nowReading = { players: message.players, at: Date.parse(message.ts) };
		});
		void instances.stats(id).then(
			(reading) => {
				if (!disposed && !received && reading.available && reading.ts) {
					nowReading = { players: reading.players, at: Date.parse(reading.ts) };
				}
			},
			() => {}
		);
		const timer = setInterval(() => (clock = Date.now()), 5_000);
		return () => {
			disposed = true;
			off();
			clearInterval(timer);
		};
	});

	function chooseDays(value: number) {
		days = value;
		fixedEnd = null;
	}
	function move(direction: -1 | 1) {
		fixedEnd = (fixedEnd ?? Date.now()) + direction * days * DAY;
	}
	function formatTime(ms: number): string {
		return new Date(ms).toLocaleString();
	}
	function formatDuration(ms: number): string {
		if (ms > 0 && ms < 60_000) return '<1 min';
		const minutes = Math.round(ms / 60_000);
		if (minutes >= 24 * 60)
			return `${Math.floor(minutes / (24 * 60))} d ${Math.floor((minutes % (24 * 60)) / 60)} h`;
		if (minutes < 60) return `${minutes} min`;
		return `${Math.floor(minutes / 60)} h ${minutes % 60} min`;
	}
	function intervalText(interval: PlayerInterval): string {
		const count = interval.players === null ? 'not observed' : `${interval.players} players`;
		return `${formatTime(interval.from)} to ${formatTime(interval.to)}: ${count} for ${formatDuration(interval.to - interval.from)}`;
	}
	function selectAt(event: PointerEvent) {
		if (!history || intervals.length === 0) return;
		const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
		const fraction = Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width));
		const at = rangeStart + fraction * duration;
		const index = intervals.findIndex((interval) => at >= interval.from && at < interval.to);
		if (index >= 0) selected = index;
	}
	function moveSelection(event: KeyboardEvent) {
		if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return;
		event.preventDefault();
		if (!intervals.length) return;
		selected = Math.max(
			0,
			Math.min(intervals.length - 1, selected + (event.key === 'ArrowRight' ? 1 : -1))
		);
	}
</script>

<Card.Root>
	<Card.Header>
		<div class="flex items-start justify-between gap-3">
			<Card.Title>Player activity</Card.Title>
			<div class="shrink-0 rounded-md border px-3 py-1 text-right">
				<p class="text-xs text-muted-foreground">Now</p>
				<p class="text-base leading-tight font-semibold tabular-nums">
					{nowStale || nowReading?.players === null ? 'unknown' : nowReading?.players}
				</p>
				{#if nowStale}<p class="text-xs text-muted-foreground">stale</p>{/if}
			</div>
		</div>
		<Card.Description
			>Estimated from counts reported while the panel watched the server log. “Not observed” means
			the count is unknown, not zero.</Card.Description
		>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<div class="flex flex-wrap items-center gap-2">
			<div class="flex flex-wrap gap-1" aria-label="Time range">
				{#each [1, 7, 30] as option (option)}
					<Button
						size="sm"
						variant={days === option ? 'default' : 'outline'}
						onclick={() => chooseDays(option)}
						>{option === 1 ? '24 hours' : `${option} days`}</Button
					>
				{/each}
			</div>
			<Button
				size="sm"
				variant="outline"
				disabled={(fixedEnd ?? clock) - days * DAY <= clock - 30 * DAY}
				onclick={() => move(-1)}
				aria-label="Previous period">Previous</Button
			>
			<Button
				size="sm"
				variant="outline"
				disabled={fixedEnd === null || fixedEnd + days * DAY > clock}
				onclick={() => move(1)}
				aria-label="Next period">Next</Button
			>
			{#if fixedEnd !== null}<Button size="sm" variant="outline" onclick={() => (fixedEnd = null)}
					>Back to live</Button
				>{/if}
		</div>
		{#if history}<p class="text-xs text-muted-foreground">
				{formatTime(rangeStart)} to {formatTime(rangeEnd)} · {timezone}
			</p>{/if}
		{#if loading && !history}<p class="text-sm text-muted-foreground">Loading activity…</p>{/if}
		{#if failure}<Problem error={failure} />
			<div class="flex items-center gap-2">
				<p class="text-sm text-muted-foreground">Activity may be stale.</p>
				<Button size="sm" variant="outline" onclick={() => (retry += 1)}>Retry activity</Button>
			</div>{/if}
		{#if history && activity}
			<div class="grid grid-cols-2 gap-3 md:grid-cols-4">
				<div class="rounded-md border p-3">
					<p class="text-xs text-muted-foreground">Peak players</p>
					<p class="text-xl font-semibold tabular-nums">{activity.peak ?? '—'}</p>
					{#if activity.peakAt !== null}<p class="text-xs text-muted-foreground">
							{new Date(activity.peakAt).toLocaleString([], {
								month: 'short',
								day: 'numeric',
								hour: 'numeric',
								minute: '2-digit'
							})}
						</p>{/if}
				</div>
				<div class="rounded-md border p-3">
					<p class="text-xs text-muted-foreground">Average players</p>
					<p class="text-xl font-semibold tabular-nums">
						{activity.average === null ? '—' : activity.average.toFixed(1)}
					</p>
				</div>
				<div class="rounded-md border p-3">
					<p class="text-xs text-muted-foreground">Time with players online</p>
					<p class="text-xl font-semibold tabular-nums">
						{activity.knownMs ? formatDuration(activity.activeMs) : '—'}
					</p>
				</div>
				<div class="rounded-md border p-3">
					<p class="text-xs text-muted-foreground">Observed player-hours</p>
					<p class="text-xl font-semibold tabular-nums">
						{activity.knownMs ? (activity.playerMs / 3_600_000).toFixed(1) : '—'}
					</p>
				</div>
			</div>
			<p class="text-sm text-muted-foreground">
				Observation coverage: {formatDuration(activity.knownMs)} ({(
					activity.coverage * 100
				).toFixed(1)}%){activity.coverage < 1 ? ' · summaries are partial' : ''}. Updated {updatedAt ===
				null
					? 'unknown'
					: formatTime(updatedAt)}.
			</p>
			<div class="flex flex-wrap gap-4 text-xs text-muted-foreground" aria-label="Chart legend">
				<span class="inline-flex items-center gap-1"
					><span class="size-3 rounded-sm bg-chart-2"></span>Observed count</span
				>
				<span class="inline-flex items-center gap-1"
					><span
						class="size-3 border border-muted-foreground/30"
						style="background: repeating-linear-gradient(135deg, transparent, transparent 5px, color-mix(in srgb, var(--muted-foreground) 18%, transparent) 5px, color-mix(in srgb, var(--muted-foreground) 18%, transparent) 7px);"
					></span>Not observed</span
				>
			</div>
			<div class="relative h-[280px] min-w-0 rounded-md border">
				{#if chartData.length}
					<AreaChart
						data={chartData}
						series={chartSeries}
						seriesLayout="overlap"
						x="t"
						y="v"
						xDomain={[rangeStart, rangeEnd]}
						yDomain={[0, yMax]}
						padding={{ left: 56, right: 16, top: 16, bottom: 40 }}
						axis={false}
						grid={false}
						tooltipContext={false}
						rule={false}
						legend={false}
					/>
				{/if}
				{#each yTicks as tick (tick)}
					<span
						class="pointer-events-none absolute left-1 w-12 -translate-y-1/2 text-right text-xs text-muted-foreground tabular-nums"
						style={`top: calc(16px + (100% - 56px) * ${1 - tick / yMax});`}>{tick}</span
					>
					<div
						class="pointer-events-none absolute right-4 left-14 border-t border-border/50"
						style={`top: calc(16px + (100% - 56px) * ${1 - tick / yMax});`}
					></div>
				{/each}
				{#each xTicks as tick (tick.part)}
					<span
						class="pointer-events-none absolute bottom-1 text-[10px] whitespace-nowrap text-muted-foreground tabular-nums {tick.part ===
						0
							? ''
							: tick.part === 1
								? '-translate-x-full'
								: '-translate-x-1/2'} {tick.part === 0.25 || tick.part === 0.75
							? 'hidden sm:block'
							: ''}"
						style={`left: calc(56px + (100% - 72px) * ${tick.part});`}
						>{days === 1
							? new Date(tick.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
							: new Date(tick.at).toLocaleDateString([], { month: 'short', day: 'numeric' })}</span
					>
				{/each}
				{#each intervals.filter((interval) => interval.players === null) as interval (interval.from)}
					<div
						class="pointer-events-none absolute z-10 border-x border-muted-foreground/30"
						style={`left: calc(56px + (100% - 72px) * ${(interval.from - rangeStart) / duration}); width: calc((100% - 72px) * ${(interval.to - interval.from) / duration}); top: 16px; bottom: 40px; background: repeating-linear-gradient(135deg, transparent, transparent 5px, color-mix(in srgb, var(--muted-foreground) 18%, transparent) 5px, color-mix(in srgb, var(--muted-foreground) 18%, transparent) 7px);`}
					></div>
				{/each}
				{#if chosen}
					<div
						class="pointer-events-none absolute z-10 border-x-2 {chosen.players === null
							? 'border-muted-foreground bg-muted-foreground/15'
							: 'border-chart-2 bg-chart-2/15'}"
						style={`left: calc(56px + (100% - 72px) * ${(chosen.from - rangeStart) / duration}); width: max(2px, calc((100% - 72px) * ${(chosen.to - chosen.from) / duration})); top: 16px; bottom: 40px;`}
					></div>
				{/if}
				<button
					type="button"
					class="absolute z-20 cursor-crosshair outline-none focus-visible:ring-2 focus-visible:ring-ring"
					style="left: 56px; right: 16px; top: 16px; bottom: 40px"
					aria-label="Player activity timeline. Use left and right arrow keys to inspect intervals."
					onpointermove={selectAt}
					onpointerdown={selectAt}
					onkeydown={moveSelection}
				></button>
			</div>
			<p class="min-h-12 rounded-md border bg-muted/30 px-3 py-2 text-sm" aria-live="polite">
				{chosen
					? intervalText(chosen)
					: 'Hover, tap, or focus the chart and use arrow keys to inspect an interval.'}
			</p>
			<details class="rounded-md border p-3">
				<summary class="cursor-pointer text-sm font-medium"
					>View observations ({intervals.length})</summary
				>
				<div class="mt-3 overflow-x-auto">
					<table class="w-full text-sm">
						<caption class="sr-only">Observed player counts, newest first</caption><thead
							><tr class="text-left text-xs text-muted-foreground"
								><th scope="col">From</th><th scope="col">To</th><th scope="col">Duration</th><th
									scope="col">Players</th
								></tr
							></thead
						><tbody>
							{#each tableRows as interval (interval.from)}<tr class="border-t"
									><td class="py-1 tabular-nums">{formatTime(interval.from)}</td><td
										class="py-1 tabular-nums">{formatTime(interval.to)}</td
									><td class="py-1 tabular-nums">{formatDuration(interval.to - interval.from)}</td
									><td class="py-1"
										>{interval.players === null ? 'not observed' : interval.players}</td
									></tr
								>{/each}
						</tbody>
					</table>
				</div>
				<div class="mt-3 flex gap-2">
					<Button
						size="sm"
						variant="outline"
						disabled={tablePage === 0}
						onclick={() => (tablePage -= 1)}>Newer observations</Button
					><Button
						size="sm"
						variant="outline"
						disabled={(tablePage + 1) * 25 >= intervals.length}
						onclick={() => (tablePage += 1)}>Older observations</Button
					>
				</div>
			</details>
		{/if}
	</Card.Content>
</Card.Root>
