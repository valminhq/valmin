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
	import RestartNotice from '$lib/components/restart-notice.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import JoinCode from '$lib/components/join-code.svelte';
	import ConsoleView from '$lib/components/console-view.svelte';
	import Sparkline from '$lib/components/sparkline.svelte';
	import UpdateNotice from '$lib/components/update-notice.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Archive from '@lucide/svelte/icons/archive';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import Package from '@lucide/svelte/icons/package';
	import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
	import Settings from '@lucide/svelte/icons/settings';

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
	const canSeeMods = $derived(allowed.includes(actions.modsList));
	const canSeeConfigs = $derived(allowed.includes(actions.configRead));
	const canSeeBackups = $derived(allowed.includes(actions.backupsList));

	async function load() {
		try {
			// The permission set is fetched at sign-in and on a `4403` close (`14 §6`), neither of
			// which fires when an instance is created, so an instance missing from it would hide
			// every control on this page. Re-read once per page load when that happens.
			if (session.allowed(id).length === 0) await session.refreshPermissions();
			instance = await instances.get(id);
			history = await instances.jobs(id);
			// Read with the page, not on the stats cadence: it is a directory walk, and the figure
			// only moves when something is installed or deleted.
			disk = canStats ? await instances.disk(id) : null;
			failure = null;
		} catch (err) {
			failure = err;
		}
	}

	// Subscribe, then fetch (G3, `14 §7.2`), and again on every reconnect: the socket cannot say
	// what changed while it was gone, and its subscriptions did not survive the close
	// (ADR-041).
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
	/** Shown as the daemon worded it, never parsed here (F2): matching on the text would be a
	 * second copy of a decision the daemon already made. */
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
				{#if inst.crossplay_join_code}
					<JoinCode code={inst.crossplay_join_code} />
				{/if}
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
			<div class="ml-auto flex flex-wrap gap-2">
				{#if canSeeBackups}
					<Button
						variant="ghost"
						size="sm"
						href={resolve('/instances/[id]/backups', { id: inst.id })}
					>
						<Archive />
						Backups
					</Button>
				{/if}
				{#if canSeeMods}
					<Button variant="ghost" size="sm" href={resolve('/instances/[id]/mods', { id: inst.id })}>
						<Package />
						Mods
					</Button>
				{/if}
				{#if canSeeConfigs}
					<Button
						variant="ghost"
						size="sm"
						href={resolve('/instances/[id]/configs', { id: inst.id })}
					>
						<SlidersHorizontal />
						Settings files
					</Button>
				{/if}
				<Button
					variant="ghost"
					size="sm"
					href={resolve('/instances/[id]/settings', { id: inst.id })}
				>
					<Settings />
					Server settings
				</Button>
			</div>
		</div>

		{#if inst.restart_required}
			<RestartNotice />
		{/if}

		<!--
			An available update is a property of the instance, not a state it is in (`12 §2.5`):
			the server keeps running and nothing changes until an operator says so.
		-->
		<UpdateNotice instance={inst} onchange={load} />

		{#if uncleanStop}
			<!--
				A stop is clean only when the panel saw the anchored save-complete line (`12 §3.4`,
				`03 §3.2.1`). Not seeing it does not mean the world is damaged, only that nobody can
				say it is not.
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
							`players` is null on every measured build and join/leave patterns are deferred
							(E7, Q7), so this reads "unknown" rather than 0. No memory alarm either: the
							cache term has not been measured (`14 §4.3`).
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
									Split by category because only one of the three is unrecoverable (`02 §5`).
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
