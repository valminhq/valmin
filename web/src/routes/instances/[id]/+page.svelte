<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import {
		actions,
		instances,
		isTransient,
		type DiskUsage,
		type CommandCapabilities,
		type Instance
	} from '$lib/api/instances';
	import { operations, type Operation } from '$lib/api/operations';
	import { scheduleKinds } from '$lib/api/schedules';
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
	import OperationNotice from '$lib/components/operation-notice.svelte';
	import RestartNotice from '$lib/components/restart-notice.svelte';
	import ConnectionSummary from '$lib/components/connection-summary.svelte';
	import ConsoleView from '$lib/components/console-view.svelte';
	import Sparkline from '$lib/components/sparkline.svelte';
	import UpdateNotice from '$lib/components/update-notice.svelte';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import Copy from '@lucide/svelte/icons/copy';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let history = $state<Job[]>([]);
	let disk = $state<DiskUsage | null>(null);
	let operation = $state<Operation | null>(null);
	let capabilities = $state<CommandCapabilities | null>(null);
	let failure = $state<unknown>(null);
	let busy = $state(false);

	const consoleBuffer = $derived(new ConsoleBuffer(id));
	const stats = $derived(new StatsWindow(id));

	const allowed = $derived(session.allowed(id));
	// Scheduling lives on the backups screen because that is where its editor is, and nothing
	// in the section names points there. Offered to whoever can schedule anything at all.
	const canSchedule = $derived(
		allowed.includes(actions.backupsList) &&
			scheduleKinds.some((kind) => allowed.includes(kind.action))
	);
	const canConsole = $derived(allowed.includes(actions.consoleRead));
	const canStats = $derived(allowed.includes(actions.statsRead));
	const canSendCommands = $derived(allowed.includes(actions.commandsSend));

	async function load() {
		try {
			// The permission set is fetched at sign-in and on a `4403` close (`14 §6`), neither of
			// which fires when an instance is created, so an instance missing from it would hide
			// every control on this page. Re-read once per page load when that happens.
			if (session.allowed(id).length === 0) await session.refreshPermissions();
			instance = await instances.get(id);
			capabilities = await instances.capabilities(id);
			history = await instances.jobs(id);
			// An instance whose definition chain never finished is stopped with a free lock, so
			// nothing else on this page would say its mods or configuration are missing (Q52).
			operation = await operations.get(id);
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
			if (!instance) return;
			// The code is latched from the log seconds after the server reaches running, so
			// nothing the page fetched on load carries it (Q25).
			if (m.type === 'join_code') {
				instance = { ...instance, crossplay_join_code: m.code };
				return;
			}
			if (m.type !== 'state') return;
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

	/** The control that was pressed, so the button that sent the request is the one that says a
	 * request is in flight. It clears when the daemon accepts the job; the state the socket then
	 * reports is what carries the rest of the transition. */
	let pending = $state('');

	async function run(action: () => Promise<unknown>, label = '') {
		pending = label;
		busy = true;
		failure = null;
		try {
			await action();
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
			pending = '';
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
	<Problem error={failure} />

	{#if instance}
		{@const inst = instance}
		<ConnectionSummary instance={inst} />

		<div
			aria-label="Server controls"
			role="group"
			class="flex flex-wrap items-center gap-2 rounded-lg border bg-card p-3"
		>
			{#if allowed.includes(actions.start)}
				<Button
					variant={inst.state === 'stopped' ? 'default' : 'outline'}
					size="sm"
					disabled={busy ||
						isTransient(inst.state) ||
						inst.state !== 'stopped' ||
						operation !== null}
					onclick={() => run(() => instances.start(inst.id), 'start')}
				>
					<Play />
					{pending === 'start' || inst.state === 'starting' ? 'Starting…' : 'Start'}
				</Button>
			{/if}
			{#if allowed.includes(actions.stop)}
				<Button
					variant="outline"
					size="sm"
					disabled={busy || isTransient(inst.state) || inst.state !== 'running'}
					onclick={() => run(() => instances.stop(inst.id), 'stop')}
				>
					<Square />
					{pending === 'stop' || inst.state === 'stopping' ? 'Stopping…' : 'Stop'}
				</Button>
			{/if}
			{#if allowed.includes(actions.restart)}
				<Button
					variant="outline"
					size="sm"
					disabled={busy || isTransient(inst.state) || inst.state !== 'running'}
					onclick={() => run(() => instances.restart(inst.id), 'restart')}
				>
					<RotateCw />
					{pending === 'restart' ? 'Restarting…' : 'Restart'}
				</Button>
			{/if}
			<div class="ml-auto flex flex-wrap gap-2">
				{#if allowed.includes(actions.clone)}
					<Button
						variant="ghost"
						size="sm"
						disabled={inst.state !== 'stopped'}
						href={resolve('/instances/[id]/clone', { id: inst.id })}
					>
						<Copy />
						Clone
					</Button>
				{/if}
			</div>
		</div>

		{#if inst.state === 'error'}
			<!--
				`error` is a parking state and its only exit is a human's (`12 §2.4`). Acknowledging
				re-runs reconciliation rather than clearing a flag, so the copy promises what that
				actually does: the row lands on whatever Docker supports now.
			-->
			<Alert.Root variant="destructive">
				<TriangleAlert />
				<Alert.Title>This server needs a check</Alert.Title>
				<Alert.Description class="grid justify-items-start gap-3">
					<span>
						The last {lastJob?.kind ?? 'operation'} failed, so Valmin parked this server and held its
						controls. Checking compares it with Docker and sets it back to stopped or running, whichever
						is true now.
					</span>
					<Button
						variant="outline"
						size="sm"
						disabled={busy}
						onclick={() =>
							run(async () => {
								await instances.acknowledge(inst.id);
								await load();
							})}
					>
						Check this server
					</Button>
				</Alert.Description>
			</Alert.Root>
		{/if}

		<OperationNotice instance={inst} {operation} onchange={load} />

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
							Null reads "unknown", never 0: the daemon sends null when it cannot tell, and
							drawing that as an empty server is the failure the null exists to prevent (E7).
							No memory alarm either: the cache term has not been measured (`14 §4.3`).
						-->
						<div class="flex items-baseline justify-between">
							<span class="text-xs text-muted-foreground">Players</span>
							<span class="text-lg font-semibold tabular-nums">
								{stats.latest?.players ?? 'unknown'}
							</span>
						</div>
						{#if disk}
							<div class="grid gap-1 border-t pt-3">
								<div class="flex items-baseline justify-between">
									<span class="text-xs text-muted-foreground">Disk</span>
									<span class="text-lg font-semibold tabular-nums">{bytes(disk.total_bytes)}</span>
								</div>
								<!--
									Free space, next to footprint rather than instead of it. The server stops
									persisting the world below its own floor without crashing or logging an
									error, so this is the figure that moves before a world is lost and
									total_bytes is the one that does not (`03 §3.4`). `low` is the panel's
									decision, not this component's, so every surface agrees.
								-->
								<div class="flex items-baseline justify-between">
									<span class="text-xs text-muted-foreground">Free</span>
									<span
										class="text-sm font-medium tabular-nums {disk.low ? 'text-destructive' : ''}"
										>{bytes(disk.free_bytes)}</span
									>
								</div>
								{#if disk.low}
									<p class="text-xs text-destructive">
										Below {bytes(disk.alarm_bytes)} free. The server stops saving the world when the disk
										fills, and it does that silently — free space here before playing further.
									</p>
								{/if}
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
					{#if canSchedule}
						<p class="text-xs text-muted-foreground">
							Backups, restarts and game updates can run on a schedule, set up under
							<a class="underline" href={resolve('/instances/[id]/backups', { id })}>Backups</a>.
						</p>
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
					<ConsoleView
						buffer={consoleBuffer}
						commandChannel={capabilities?.command_channel ?? 'none'}
						canSend={canSendCommands}
						running={inst.state === 'running'}
					/>
				{:else}
					<p class="text-sm text-muted-foreground">Not available to you.</p>
				{/if}
			</Card.Content>
		</Card.Root>
	{:else if !failure}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{/if}
</div>
