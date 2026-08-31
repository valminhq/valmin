<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import {
		actions,
		instances,
		isTransient,
		type DiskUsage,
		type Instance
	} from '$lib/api/instances';
	import type { Job } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { ConsoleBuffer } from '$lib/state/console.svelte';
	import { StatsWindow } from '$lib/state/stats.svelte';
	import { socket, socketStatus } from '$lib/socket/index.svelte';
	import { topics, type ServerMessage } from '$lib/socket/messages';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Alert from '$lib/components/ui/alert';
	import Problem from '$lib/components/problem.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ConsoleView from '$lib/components/console-view.svelte';
	import Sparkline from '$lib/components/sparkline.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let history = $state<Job[]>([]);
	let disk = $state<DiskUsage | null>(null);
	let failure = $state<unknown>(null);
	let busy = $state(false);

	const consoleBuffer = $derived(new ConsoleBuffer(id));
	const stats = $derived(new StatsWindow(id));

	const allowed = $derived(session.allowed(id));
	const canConsole = $derived(allowed.includes(actions.consoleRead));
	const canStats = $derived(allowed.includes(actions.statsRead));

	async function load() {
		try {
			instance = await instances.get(id);
			history = await instances.jobs(id);
			// `↯` Read with the page, not on the stats cadence. It is a directory walk, and
			// a figure that only moves when something is installed or deleted does not
			// belong behind a two-second poll.
			disk = canStats ? await instances.disk(id) : null;
			failure = null;
		} catch (err) {
			failure = err;
		}
	}

	// `↯` Subscribe, then fetch (G3, `14 §7.2`), and again on every reconnect: the socket
	// cannot say what changed while it was gone, and its subscriptions did not survive the
	// close (ADR-041).
	$effect(() => {
		const off = socket.subscribe(topics.state(id), (m: ServerMessage) => {
			if (m.type !== 'state' || !instance) return;
			instance = { ...instance, state: m.state, restart_required: m.restart_required };
			// A transition finished; the job that drove it is what carries the warning.
			void instances.jobs(id).then((rows) => (history = rows));
		});
		void load();
		return off;
	});

	let lastStatus = $state(socketStatus.value);
	$effect(() => {
		const status = socketStatus.value;
		if (status === 'open' && lastStatus !== 'open') void load();
		lastStatus = status;
	});

	$effect(() => (canConsole ? consoleBuffer.open() : undefined));
	$effect(() => (canStats ? stats.open() : undefined));

	const lastJob = $derived(history[0] ?? null);
	/** `↯` The panel's own words, not a pattern the frontend matches (F2). ADR-043's
	 * `running (registration unconfirmed)` reaches an operator by being shown, not by being
	 * parsed — a substring check here would be a second, weaker copy of a decision the daemon
	 * already made, and it would rot the day the wording changes. */
	const lastMessage = $derived(lastJob?.message ?? null);
	/** `clean` is a typed field (`12 §3.4`), so this one is a real branch: the server stopped
	 * without the save-complete line ever being seen. */
	const uncleanStop = $derived(history.find((j) => j.clean === false) ?? null);

	async function run(action: () => Promise<unknown>) {
		busy = true;
		failure = null;
		try {
			await action();
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	function bytes(n: number | null | undefined): string | null {
		if (n === null || n === undefined) return null;
		const units = ['B', 'KiB', 'MiB', 'GiB'];
		let v = n;
		let u = 0;
		while (v >= 1024 && u < units.length - 1) {
			v /= 1024;
			u += 1;
		}
		return `${v.toFixed(u === 0 ? 0 : 1)} ${units[u]}`;
	}

	function pct(n: number | null | undefined): string | null {
		return n === null || n === undefined ? null : `${n.toFixed(1)}%`;
	}
</script>

<div class="mx-auto grid max-w-4xl gap-4 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft />
		Servers
	</Button>

	<Problem error={failure} />

	{#if instance}
		{@const inst = instance}
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h1 class="text-lg font-semibold">{inst.name}</h1>
				<p class="text-sm text-muted-foreground">
					{inst.server_name} · world {inst.world_name} · udp {inst.base_port}–{inst.base_port + 1}
				</p>
			</div>
			<StateBadge state={inst.state} restartRequired={inst.restart_required} />
		</div>

		<div class="flex flex-wrap gap-2">
			{#if allowed.includes(actions.start)}
				<Button
					variant="outline"
					size="sm"
					disabled={busy || isTransient(inst.state) || inst.state !== 'stopped'}
					onclick={() => run(() => instances.start(inst.id))}
				>
					<Play />
					Start
				</Button>
			{/if}
			{#if allowed.includes(actions.stop)}
				<Button
					variant="outline"
					size="sm"
					disabled={busy || isTransient(inst.state) || inst.state !== 'running'}
					onclick={() => run(() => instances.stop(inst.id))}
				>
					<Square />
					Stop
				</Button>
			{/if}
			{#if allowed.includes(actions.restart)}
				<Button
					variant="outline"
					size="sm"
					disabled={busy || isTransient(inst.state) || inst.state !== 'running'}
					onclick={() => run(() => instances.restart(inst.id))}
				>
					<RotateCw />
					Restart
				</Button>
			{/if}
		</div>

		{#if inst.restart_required}
			<Alert.Root>
				<TriangleAlert />
				<Alert.Title>Restart required</Alert.Title>
				<Alert.Description>
					Settings changed since this server started. The running server is still using the old
					ones.
				</Alert.Description>
			</Alert.Root>
		{/if}

		{#if uncleanStop}
			<!--
				`↯` `12 §3.4`, `03 §3.2.1`. The panel waits for the anchored literal
				`World save writing finished` before it calls a stop clean. Not seeing it does not
				mean the world is damaged — but it does mean nobody can say it is not, and that is
				what an operator has to be told before they decide whether to keep playing.
			-->
			<Alert.Root variant="destructive">
				<TriangleAlert />
				<Alert.Title>The last stop was not confirmed</Alert.Title>
				<Alert.Description>
					The server exited without the panel seeing the world save finish. The world is probably
					intact, but this stop cannot be confirmed as clean.
				</Alert.Description>
			</Alert.Root>
		{/if}

		<div class="grid gap-4 md:grid-cols-2">
			<Card.Root>
				<Card.Header>
					<Card.Title>Resources</Card.Title>
					<Card.Description>
						{#if stats.latest?.available}
							Sampled every 2 seconds.
						{:else}
							Nothing is being sampled — the server is not running.
						{/if}
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-3">
					{#if !canStats}
						<p class="text-sm text-muted-foreground">Not available to you.</p>
					{:else}
						<Sparkline
							samples={stats.samples}
							pick={(s) => s.cpu}
							label="CPU"
							value={pct(stats.latest?.cpu_pct)}
							max={100}
						/>
						<Sparkline
							samples={stats.samples}
							pick={(s) => s.mem}
							label="Memory"
							value={bytes(stats.latest?.mem_bytes)}
						/>
						<!--
							`↯` E7, Q7. `players` is null on every build the panel has measured, and
							join/leave patterns were deliberately deferred past 1.0. "Unknown" is
							honest; a hardcoded pattern silently reporting 0 forever is the failure
							being avoided. There is deliberately no memory alarm either (`14 §4.3`):
							nobody has measured the cache term on a server up for days.
						-->
						<div class="flex items-baseline justify-between">
							<span class="text-xs text-muted-foreground">Players</span>
							<span class="text-sm font-medium">unknown</span>
						</div>
						{#if disk}
							<div class="grid gap-1 border-t pt-3">
								<div class="flex items-baseline justify-between">
									<span class="text-xs text-muted-foreground">Disk</span>
									<span class="text-sm font-medium tabular-nums">{bytes(disk.total_bytes)}</span>
								</div>
								<!--
									The split is the point: after "am I out of space" comes "what can I
									delete", and only one of these three is unrecoverable (`02 §5`).
								-->
								<dl class="grid grid-cols-3 gap-2 text-xs text-muted-foreground">
									<div>
										<dt>world</dt>
										<dd class="tabular-nums">{bytes(disk.worlds_bytes)}</dd>
									</div>
									<div>
										<dt>server</dt>
										<dd class="tabular-nums">{bytes(disk.server_bytes)}</dd>
									</div>
									<div>
										<dt>backups</dt>
										<dd class="tabular-nums">{bytes(disk.backups_bytes)}</dd>
									</div>
								</dl>
							</div>
						{/if}
					{/if}
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Last operation</Card.Title>
					<Card.Description>
						{lastJob ? `${lastJob.kind} · ${lastJob.status}` : 'Nothing has run yet.'}
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-2 text-sm">
					{#if lastMessage}
						<p>{lastMessage}</p>
					{/if}
					{#if lastJob?.error}
						<p class="text-destructive">{lastJob.error}</p>
					{/if}
					{#if history.length > 1}
						<ul class="grid gap-1 text-xs text-muted-foreground">
							{#each history.slice(1, 6) as job (job.job_id)}
								<li>{job.kind} · {job.status}</li>
							{/each}
						</ul>
					{/if}
				</Card.Content>
			</Card.Root>
		</div>

		<Card.Root>
			<Card.Header>
				<Card.Title>Console</Card.Title>
			</Card.Header>
			<Card.Content>
				{#if canConsole}
					<ConsoleView buffer={consoleBuffer} />
				{:else}
					<p class="text-sm text-muted-foreground">Not available to you.</p>
				{/if}
			</Card.Content>
		</Card.Root>
	{:else if !failure}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{/if}
</div>
