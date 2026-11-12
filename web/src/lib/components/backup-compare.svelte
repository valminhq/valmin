<script lang="ts">
	import { resolve } from '$app/paths';
	import { formatInstant } from '#lib/api/schedules.js';
	import type { Backup } from '#lib/api/backups.js';
	import { actions, instances } from '#lib/api/instances.js';
	import { auditLog, type AuditEntry } from '#lib/api/admin.js';
	import { playerHistory, type PlayerObservation } from '#lib/api/players.js';
	import type { Job } from '#lib/api/types.js';
	import { describe } from '#lib/audit.js';
	import { localDay, playersRetained, worldChange } from '#lib/backup-compare.js';
	import { formatBytes } from '#lib/format.js';
	import { formatDuration } from '#lib/job-history.js';
	import { playerActivity, type PlayerActivity } from '#lib/player-activity.js';
	import { session } from '#lib/state/session.svelte.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import JobHistory from '#lib/components/job-history.svelte';
	import Problem from '#lib/components/problem.svelte';
	import { warningText } from '#lib/tone.js';

	let {
		instanceId,
		pair,
		list,
		open = $bindable(false)
	}: {
		instanceId: string;
		/** The two archives to compare, in either order. */
		pair: [Backup, Backup];
		/** The loaded catalogue, for the archives taken in between. */
		list: Backup[];
		open?: boolean;
	} = $props();

	const PAGE = 50;
	const AUDIT_PAGES = 10;
	const change = $derived(worldChange(pair[0], pair[1], list));
	const since = $derived(change.older.created_at);
	const until = $derived(change.newer.created_at);
	const canStats = $derived(session.allowed(instanceId).includes(actions.statsRead));
	const canAudit = $derived(session.allowedGlobally().includes(actions.auditRead));
	const retained = $derived(playersRetained(since, until, Date.now()));
	const auditHref = $derived(
		resolve(
			`admin/audit?${new URLSearchParams({ instance_id: instanceId, from: localDay(since), to: localDay(until) })}`
		)
	);

	/** Bumped for every window loaded, so a response for an earlier window is dropped. */
	let generation = 0;
	let jobs = $state<Job[]>([]);
	let jobCursor = $state<string | null>(null);
	let jobsLoading = $state(true);
	let jobsFailure = $state<unknown>(null);
	let loadingMore = $state(false);
	let players = $state<PlayerActivity | null>(null);
	let playersFailure = $state<unknown>(null);
	let audit = $state<AuditEntry[] | null>(null);
	let auditMore = $state(false);
	let auditFailure = $state<unknown>(null);

	/** Loads each section the viewer may see for the window, each failing on its own. */
	$effect(() => {
		if (!open) return;
		const window = { since, until };
		const mine = ++generation;
		const current = () => mine === generation;
		jobs = [];
		jobCursor = null;
		jobsLoading = true;
		jobsFailure = null;
		players = null;
		playersFailure = null;
		audit = null;
		auditMore = false;
		auditFailure = null;

		instances
			.jobPage(instanceId, PAGE, null, window)
			.then((page) => {
				if (!current()) return;
				jobs = page.items;
				jobCursor = page.next_cursor;
			})
			.catch((err) => current() && (jobsFailure = err))
			.finally(() => current() && (jobsLoading = false));
		if (canStats && retained) {
			loadPlayers(window.since, window.until)
				.then((activity) => current() && (players = activity))
				.catch((err) => current() && (playersFailure = err));
		}
		if (canAudit) {
			loadAudit(window)
				.then(([entries, more]) => {
					if (!current()) return;
					audit = entries;
					auditMore = more;
				})
				.catch((err) => current() && (auditFailure = err));
		}
		return () => {
			generation++;
		};
	});

	async function loadPlayers(from: string, to: string): Promise<PlayerActivity> {
		const first = await playerHistory.range(instanceId, from, to);
		const points: PlayerObservation[] = [...first.items];
		for (let cursor = first.next_cursor; cursor;) {
			const page = await playerHistory.range(instanceId, first.range.from, first.range.to, cursor);
			points.push(...page.items);
			cursor = page.next_cursor;
		}
		return playerActivity(first.range, points);
	}

	/** Reads up to AUDIT_PAGES pages of the window's audit entries, keeping those that are not
	 * jobs, since the operations list already shows those. Reports whether entries remain. */
	async function loadAudit(window: {
		since: string;
		until: string;
	}): Promise<[AuditEntry[], boolean]> {
		const entries: AuditEntry[] = [];
		let cursor: string | undefined;
		for (let i = 0; i < AUDIT_PAGES; i++) {
			const page = await auditLog.list({ instance_id: instanceId, ...window }, cursor);
			entries.push(...page.items.filter((entry) => !entry.job_id));
			if (!page.next_cursor) return [entries, false];
			cursor = page.next_cursor;
		}
		return [entries, true];
	}

	async function moreJobs() {
		if (!jobCursor || loadingMore) return;
		const mine = generation;
		loadingMore = true;
		try {
			const page = await instances.jobPage(instanceId, PAGE, jobCursor, { since, until });
			if (mine !== generation) return;
			jobs = [...jobs, ...page.items];
			jobCursor = page.next_cursor;
		} catch (err) {
			if (mine === generation) jobsFailure = err;
		} finally {
			loadingMore = false;
		}
	}

	function signed(n: number): string {
		return n < 0 ? `−${formatBytes(-n)}` : `+${formatBytes(n)}`;
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Content class="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
		<Dialog.Header>
			<Dialog.Title>Between two backups</Dialog.Title>
			<Dialog.Description>
				From {formatInstant(since)} to {formatInstant(until)}, {formatDuration(
					Date.parse(until) - Date.parse(since)
				)}. Built from what the panel recorded; the world files themselves are not compared.
			</Dialog.Description>
		</Dialog.Header>

		<div class="grid gap-4">
			<section class="grid gap-1 rounded-lg border p-4" data-testid="compare-world">
				<h3 class="text-sm font-medium">World</h3>
				<p class="text-sm">
					{#if change.deltaBytes === null}
						World size was not measured for both backups.
					{:else}
						{formatBytes(change.older.world_bytes ?? 0)} → {formatBytes(
							change.newer.world_bytes ?? 0
						)}
						<span class="text-muted-foreground tabular-nums">({signed(change.deltaBytes)})</span>
					{/if}
				</p>
				<p class="text-sm text-muted-foreground">
					{change.between.length
						? `${change.between.length} other backup${change.between.length === 1 ? '' : 's'} taken in between.`
						: 'No other backups taken in between.'}
				</p>
				{#if change.newFindings.length}
					<ul class={['list-disc pl-5 text-sm', warningText]}>
						{#each change.newFindings as finding (finding.code)}
							<li>{finding.message}</li>
						{/each}
					</ul>
				{/if}
			</section>

			{#if canStats}
				<section class="grid gap-1 rounded-lg border p-4" data-testid="compare-players">
					<h3 class="text-sm font-medium">Players</h3>
					{#if !retained}
						<p class="text-sm text-muted-foreground">
							Player counts are kept for 30 days, so this period is not covered.
						</p>
					{:else if playersFailure}
						<Problem error={playersFailure} />
					{:else if !players}
						<p class="text-sm text-muted-foreground">Loading…</p>
					{:else}
						<p class="text-sm">
							Peak {players.peak ?? '—'} · {(players.playerMs / 3_600_000).toFixed(1)} player-hours ·
							someone online for {formatDuration(players.activeMs)}
						</p>
						<p class="text-xs text-muted-foreground">
							Observed for {(players.coverage * 100).toFixed(0)}% of the period.
						</p>
					{/if}
				</section>
			{/if}

			{#if jobsLoading}
				<p class="text-sm text-muted-foreground">Loading operations…</p>
			{:else if jobsFailure && !jobs.length}
				<Problem error={jobsFailure} />
			{:else}
				<JobHistory
					{instanceId}
					{jobs}
					more={!!jobCursor}
					{loadingMore}
					onmore={() => void moreJobs()}
					empty="No operations finished between these backups."
				/>
				<Problem error={jobsFailure} />
			{/if}

			{#if canAudit}
				<section class="grid gap-2 rounded-lg border p-4" data-testid="compare-audit">
					<h3 class="text-sm font-medium">Other changes</h3>
					{#if auditFailure}
						<Problem error={auditFailure} />
					{:else if !audit}
						<p class="text-sm text-muted-foreground">Loading…</p>
					{:else if audit.length}
						<ul class="grid gap-1 text-sm">
							{#each audit as entry (entry.id)}
								<li>
									<span class="text-muted-foreground">{formatInstant(entry.created_at)}</span>
									{describe(entry)}
								</li>
							{/each}
						</ul>
					{:else if !auditMore}
						<p class="text-sm text-muted-foreground">No other changes were recorded.</p>
					{/if}
					{#if auditMore}
						<p class="text-sm text-muted-foreground">
							The period has more entries than shown here.
							<a class="underline" href={auditHref}>Open the audit log for these days</a>
						</p>
					{/if}
				</section>
			{/if}
		</div>
	</Dialog.Content>
</Dialog.Root>
