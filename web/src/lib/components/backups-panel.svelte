<script lang="ts">
	import { resolve } from '$app/paths';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { scheduleKinds, formatInstant } from '#lib/api/schedules.js';
	import { backups, type Backup, type BackupMode, type BackupRemoval } from '#lib/api/backups.js';
	import { backupAge, recoveryPoints } from '#lib/backup-points.js';
	import { formatBytes } from '#lib/format.js';
	import { actions, instances, stateSentence, type Instance } from '#lib/api/instances.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import WorldsOnDisk from '#lib/components/worlds-on-disk.svelte';
	import BackupCompare from '#lib/components/backup-compare.svelte';
	import RemoteBackupsPanel from '#lib/components/remote-backups-panel.svelte';
	import WorldImport from '#lib/components/world-import.svelte';
	import WorldImportBackup from '#lib/components/world-import-backup.svelte';
	import { remoteBackups, type RemoteSummary } from '#lib/api/remote-backups.js';
	import JobProgress from '#lib/components/job-progress.svelte';
	import Problem from '#lib/components/problem.svelte';
	import SaveFooter from '#lib/components/save-footer.svelte';
	import Download from '@lucide/svelte/icons/download';
	import History from '@lucide/svelte/icons/history';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import GitCompare from '@lucide/svelte/icons/git-compare';
	import { warningBox } from '#lib/tone.js';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let list = $state<Backup[]>([]);
	let removedByConsistent = $state<BackupRemoval[]>([]);
	let removedByBestEffort = $state<BackupRemoval[]>([]);
	let cursor = $state<string | null>(null);
	let loadingMore = $state(false);
	let moreFailure = $state<unknown>(null);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let loadFailure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let uploading = $state<string | null>(null);
	let remoteRefresh = $state(0);
	let remote = $state<RemoteSummary | null>(null);
	async function uploadRemote(archive: Backup) {
		uploading = archive.id;
		try {
			await remoteBackups.upload(instance.id, archive.id);
			failure = null;
			remoteRefresh++;
		} catch (err) {
			failure = err;
		} finally {
			uploading = null;
		}
	}
	// The dialog owns its own open flag and writes it back on cancel, so the archive being
	// acted on is tracked beside it rather than inferred from it.
	let restoring = $state<Backup | null>(null);
	let restoreOpen = $state(false);
	let deleting = $state<Backup | null>(null);
	let deleteOpen = $state(false);

	let comparing = $state(false);
	let pickedIds = $state<string[]>([]);
	let compareOpen = $state(false);
	/** The picked archives as the catalogue holds them now; a deleted one drops out. */
	const picked = $derived(list.filter((b) => pickedIds.includes(b.id)));
	/** Closes the comparison when a picked archive leaves the catalogue. */
	$effect(() => {
		if (picked.length < 2) compareOpen = false;
	});
	/** Keeps at most the two newest picks, so a third tick replaces the oldest. */
	function pick(archive: Backup, on: boolean) {
		const rest = pickedIds.filter((id) => picked.some((b) => b.id === id) && id !== archive.id);
		pickedIds = on ? [...rest, archive.id].slice(-2) : rest;
	}

	let keepCold = $state<number | null>(0);
	let keepHot = $state<number | null>(0);
	let onRestart = $state(false);
	let savingPolicy = $state(false);

	const allowed = $derived(session.allowed(instance.id));
	const canSchedule = $derived(scheduleKinds.some((kind) => allowed.includes(kind.action)));
	const canList = $derived(allowed.includes(actions.backupsList));
	const canSetPolicy = $derived(allowed.includes(actions.settings));
	const canCreate = $derived(allowed.includes(actions.backupsCreate));
	const canDownload = $derived(allowed.includes(actions.backupsDownload));
	const canRestore = $derived(allowed.includes(actions.backupsRestore));

	$effect(() => {
		if (canList) void load(instance.id);
	});

	/** Takes the daemon's row as the retention form's baseline, so the screen never shows a
	 * count it merely sent (F4). Re-runs when the parent re-reads the row after a save. */
	$effect(resetPolicy);

	async function load(id: string) {
		loading = true;
		loadFailure = null;
		moreFailure = null;
		try {
			const page = await backups.list(id);
			list = page.items;
			removedByConsistent = page.removed_by_next_consistent;
			removedByBestEffort = page.removed_by_next_best_effort;
			cursor = page.next_cursor;
			loadFailure = null;
		} catch (err) {
			loadFailure = err;
		} finally {
			loading = false;
		}
	}

	/** Appends the next page rather than replacing it, so reaching the oldest archive never
	 * costs the operator the entries already on screen. */
	async function loadMore() {
		if (!cursor || loadingMore) return;
		loadingMore = true;
		moreFailure = null;
		try {
			const page = await backups.list(instance.id, cursor);
			list = [...list, ...page.items];
			cursor = page.next_cursor;
		} catch (err) {
			moreFailure = err;
		} finally {
			loadingMore = false;
		}
	}

	/**
	 * Nothing here predicts an outcome (F4). A run finishing re-reads the catalogue, so what is
	 * on screen is what the daemon has, including an archive that failed verification and was
	 * never written.
	 */
	function finished() {
		jobRunning = false;
		void load(instance.id);
		onchange?.();
	}

	// True from a job's click until its submission answers; a second click in that window is dropped.
	let sending = $state(false);

	async function run(call: () => Promise<{ job_id: string }>) {
		if (sending) return;
		sending = true;
		failure = null;
		try {
			const job = await call();
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			failure = err;
		} finally {
			sending = false;
		}
	}

	const take = (mode: BackupMode) => run(() => backups.create(instance.id, mode));
	// A quiesced backup of a running server takes it offline, so it is confirmed like Stop.
	let confirmStop = $state(false);
	/** What retention deletes when the next backup is taken, said in one sentence, or ''. */
	function removalNote(removals: BackupRemoval[]): string {
		if (removals.length === 0) return '';
		const newest = when(removals[0].created_at);
		return removals.length === 1
			? `Retention then removes the backup from ${newest}.`
			: `Retention then removes ${removals.length} backups, the newest from ${newest}.`;
	}
	const consistentRemoval = $derived(removalNote(removedByConsistent));
	// A copy of a server that is not running is stored as a consistent one.
	const bestEffortRemoval = $derived(
		removalNote(instance.state === 'running' ? removedByBestEffort : removedByConsistent)
	);

	let acknowledging = $state<string | null>(null);
	/** Clears a flagged archive's findings, then re-reads the catalogue: acknowledging moves
	 * which archive retention protects, so the pending-deletion badges can change too. */
	async function acknowledge(archive: Backup) {
		acknowledging = archive.id;
		failure = null;
		try {
			await backups.acknowledge(instance.id, archive.id);
			await load(instance.id);
		} catch (err) {
			failure = err;
		} finally {
			acknowledging = null;
		}
	}

	function askRestore(archive: Backup) {
		restoring = archive;
		restoreOpen = true;
	}

	function askDelete(archive: Backup) {
		deleting = archive;
		deleteOpen = true;
	}

	function restore() {
		const target = restoring;
		if (target) void run(() => backups.restore(instance.id, target.id));
	}

	async function remove() {
		const target = deleting;
		if (!target) return;
		failure = null;
		try {
			await backups.remove(instance.id, target.id);
			await load(instance.id);
			onchange?.();
		} catch (err) {
			failure = err;
		}
	}

	/** The newest archive and the newest consistent one among those loaded. */
	const points = $derived(recoveryPoints(list));

	/** The clock backup ages are measured against, refreshed every minute. */
	let now = $state(Date.now());
	$effect(() => {
		const tick = setInterval(() => (now = Date.now()), 60_000);
		return () => clearInterval(tick);
	});

	const policyChanged = $derived(
		keepCold !== instance.backup_keep_cold ||
			keepHot !== instance.backup_keep_hot ||
			onRestart !== instance.backup_on_restart
	);
	unsaved(() => policyChanged);
	const policyValid = $derived(
		keepCold !== null &&
			Number.isInteger(keepCold) &&
			keepCold >= 0 &&
			keepHot !== null &&
			Number.isInteger(keepHot) &&
			keepHot >= 0
	);

	/** Puts the retention fields back on the daemon's row. */
	function resetPolicy() {
		keepCold = instance.backup_keep_cold;
		keepHot = instance.backup_keep_hot;
		onRestart = instance.backup_on_restart;
	}

	/**
	 * Saves retention and re-reads the catalogue, because changing a count changes which
	 * archives are marked for the next prune. It writes columns and deletes nothing on its own:
	 * retention is applied by the next backup or prune run (`02 §4.4` step 7).
	 */
	async function savePolicy() {
		const cold = keepCold;
		const hot = keepHot;
		if (cold === null || hot === null || !policyValid) return;
		savingPolicy = true;
		failure = null;
		try {
			// Counts need backups.restore, since lowering one deletes archives; the restart
			// switch needs instance.settings. Each is sent only by someone who may change it.
			const row = await instances.patch(instance.id, {
				...(canRestore ? { backup_keep_cold: cold, backup_keep_hot: hot } : {}),
				...(canSetPolicy ? { backup_on_restart: onRestart } : {})
			});
			keepCold = row.backup_keep_cold;
			keepHot = row.backup_keep_hot;
			onRestart = row.backup_on_restart;
			await load(instance.id);
			onchange?.();
		} catch (err) {
			failure = err;
		} finally {
			savingPolicy = false;
		}
	}

	/** Why a quiesced backup or a restore cannot start right now, or null. The daemon refuses
	 * a restore into a server that is not stopped; a quiesced backup stops one itself. */
	const busy = $derived.by(() => {
		if (jobRunning || sending) return 'A job is running on this server. Wait for it to finish.';
		if (instance.state !== 'running' && instance.state !== 'stopped') {
			return stateSentence(instance.state);
		}
		return null;
	});
	const restoreBlocked = $derived.by(() => {
		if (busy) return busy;
		if (instance.state !== 'stopped') return 'Stop this server to restore a world into it.';
		return null;
	});

	function when(iso: string): string {
		return formatInstant(iso);
	}
</script>

{#snippet point(label: string, archive: Backup, note: string)}
	<div class="grid gap-0.5 rounded-lg border bg-muted/40 p-4">
		<span class="text-xs text-muted-foreground">{label}</span>
		<span class="font-medium">
			{when(archive.created_at)}
			<span class="font-normal text-muted-foreground">
				· {backupAge(archive.created_at, now)}
			</span>
		</span>
		<span class="text-xs text-muted-foreground">
			{note} · {formatBytes(archive.size_bytes)} · {archive.trigger.replaceAll('_', ' ')}
		</span>
	</div>
{/snippet}

<div
	class="grid items-start gap-6 {(canList && (canSetPolicy || canRestore)) || canSchedule
		? 'xl:grid-cols-[minmax(0,1fr)_22rem]'
		: ''}"
>
	<div class="grid min-w-0 gap-6">
		<Card.Root class="min-w-0">
			<Card.Header>
				<Card.Title>Backup history</Card.Title>
				<Card.Description>
					Archives of this server's world, newest first. Restoring one replaces the entire
					world-save directory (worlds_local), including any other worlds in it. Mods,
					configuration, server files and player lists are not touched.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				{#if !canList}
					<p class="text-sm text-muted-foreground" data-testid="backups-blocked">
						Backups are not available to you.
					</p>
				{:else}
					{#if !loading && !loadFailure}
						{#if points.newest}
							{@const sameArchive = points.newestConsistent?.id === points.newest.id}
							<div
								class="grid gap-3 {sameArchive ? '' : 'sm:grid-cols-2'}"
								data-testid="recovery-points"
							>
								{@render point(
									'Newest backup',
									points.newest,
									sameArchive
										? 'Consistent, so also the newest consistent backup'
										: 'Best-effort, copied while running'
								)}
								{#if !sameArchive}
									{#if points.newestConsistent}
										{@render point(
											'Newest consistent backup',
											points.newestConsistent,
											'Consistent'
										)}
									{:else}
										<div
											class={['grid gap-0.5 rounded-lg border p-4', warningBox]}
											data-testid="no-consistent-backup"
										>
											<span class="text-xs">Newest consistent backup</span>
											<span class="font-medium">None</span>
											<span class="text-xs">
												{#if cursor}
													None among the backups loaded so far. Load older backups to look further.
												{:else}
													Every backup here was copied while the server was running and may not
													restore.{canCreate ? ' Use Stop and back up to make one.' : ''}
												{/if}
											</span>
										</div>
									{/if}
								{/if}
							</div>
						{:else}
							<div class="grid gap-0.5 rounded-lg border bg-muted/40 p-4">
								<span class="text-xs text-muted-foreground">Newest backup</span>
								<span class="font-medium">Nothing has been backed up yet</span>
								<span class="text-xs text-muted-foreground">
									No panel backup archives yet. Check Worlds on disk below for other saved worlds.
								</span>
							</div>
						{/if}
					{/if}
					{#if canCreate}
						<!--
					The two controls are not two speeds of the same thing, and the copy is the only
					place that difference is visible. A quiesced archive stops the server, which is what
					makes it trustworthy; a hot copy reads a world that is being written to, so it is
					best-effort and never the recommended one.
				-->
						<div class="grid gap-3 sm:grid-cols-2">
							<div class="grid gap-2 rounded-lg border p-4">
								<p class="text-sm font-medium">Back up now</p>
								<p class="text-sm text-muted-foreground">
									A running server is stopped once its world has saved and started again afterwards;
									it is offline for the whole backup. A stopped server stays stopped.
								</p>
								{#if consistentRemoval}
									<p class="text-sm text-muted-foreground">{consistentRemoval}</p>
								{/if}
								<Button
									size="sm"
									class="justify-self-start"
									disabled={busy !== null}
									onclick={() =>
										instance.state === 'running' ? (confirmStop = true) : take('quiesced')}
								>
									Stop and back up
								</Button>
							</div>
							<div class="grid gap-2 rounded-lg border p-4">
								<p class="text-sm font-medium">Back up without stopping</p>
								<p class="text-sm text-muted-foreground">
									Copies the world while the server runs. The backup may contain an incomplete save
									and may not be restorable.
								</p>
								{#if bestEffortRemoval}
									<p class="text-sm text-muted-foreground">{bestEffortRemoval}</p>
								{/if}
								<Button
									variant="outline"
									size="sm"
									class="justify-self-start"
									disabled={busy !== null}
									onclick={() => take('hot')}
								>
									Back up without stopping
								</Button>
							</div>
						</div>
					{/if}

					<Problem error={failure} />

					{#if jobId}
						<!--
					A quiesced backup is a stop, a copy of the whole world and a start, which on a large
					world is minutes with nothing else to see. A restore is the same again. F4: this is the
					daemon's own job, not a bar that runs ahead of it.
				-->
						<div class="rounded-lg border p-4">
							<JobProgress {jobId} onfinish={finished} />
						</div>
					{/if}

					{#if busy}
						<p class="text-sm text-muted-foreground">{busy}</p>
					{/if}

					{#if loading}
						<p class="text-sm text-muted-foreground">Loading…</p>
					{:else if loadFailure}
						<Problem error={loadFailure} />
						<Button variant="outline" class="justify-self-start" onclick={() => load(instance.id)}
							>Retry loading backups</Button
						>
					{:else if list.length > 0}
						{#if restoreBlocked && canRestore && !busy}
							<p class="text-sm text-muted-foreground">{restoreBlocked}</p>
						{/if}
						<!--
						One rendering at both widths. Every field carries its own label, shown on the card at
						narrow widths and replaced by the column header above md, so a row cannot say one
						thing on a phone and another on a laptop.
					-->
						{#if list.length > 1}
							<div class="flex flex-wrap items-center gap-2">
								<Button
									variant="outline"
									size="sm"
									onclick={() => {
										comparing = !comparing;
										pickedIds = [];
									}}
								>
									<GitCompare />
									<span>{comparing ? 'Cancel comparing' : 'Compare two backups'}</span>
								</Button>
								{#if comparing}
									<Button
										size="sm"
										disabled={picked.length !== 2}
										onclick={() => (compareOpen = true)}
									>
										Show what happened between them
									</Button>
									<span class="text-sm text-muted-foreground">{picked.length} of 2 selected</span>
								{/if}
							</div>
						{/if}
						<div class="grid gap-2 md:grid-cols-[minmax(0,1.3fr)_9rem_minmax(0,1fr)_5rem]">
							<div
								class="hidden gap-3 px-3 text-xs font-medium text-muted-foreground md:col-span-full md:grid md:grid-cols-subgrid"
							>
								<span>Created</span>
								<span>Consistency</span>
								<span>Reason</span>
								<span>Size</span>
							</div>
							<ul class="grid gap-2 md:contents">
								{#each list as archive (archive.id)}
									<li
										class="grid gap-3 rounded-lg border p-3 md:col-span-full md:grid-cols-subgrid md:items-baseline"
									>
										<div class="grid gap-0.5">
											<span class="text-xs text-muted-foreground md:hidden">Created</span>
											{#if comparing}
												<label class="flex items-center gap-2">
													<input
														type="checkbox"
														class="size-4 accent-primary"
														checked={picked.some((b) => b.id === archive.id)}
														onchange={(e) => pick(archive, e.currentTarget.checked)}
													/>
													<span>{when(archive.created_at)}</span>
												</label>
											{:else}
												<span>{when(archive.created_at)}</span>
											{/if}
											<span class="font-mono text-xs text-muted-foreground"
												>{archive.world_name}</span
											>
										</div>
										<div class="grid justify-items-start gap-0.5">
											<span class="text-xs text-muted-foreground md:hidden">Consistency</span>
											<!--
											Which of the two an archive is decides whether it can be trusted to
											restore, so it is on every row rather than only on the row that lacks it.
										-->
											{#if !archive.consistent}
												<Badge variant="secondary">Best-effort</Badge>
												<span class="text-xs text-muted-foreground">copied while running</span>
											{:else}
												<Badge variant="outline">Consistent</Badge>
												<span class="text-xs text-muted-foreground">server was stopped</span>
											{/if}
										</div>
										<div class="grid justify-items-start gap-0.5">
											<span class="text-xs text-muted-foreground md:hidden">Reason</span>
											<Badge variant="outline">{archive.trigger.replaceAll('_', ' ')}</Badge>
											{#if archive.prunes_next}
												<!-- A retention setting whose effect is invisible until it deletes
											     something is the wrong shape for world data. -->
												<Badge variant="destructive">Pending deletion</Badge>
												<span class="text-xs text-muted-foreground">removed at the next prune</span>
											{/if}
											{#if archive.kept_for_setup}
												<Badge variant="outline">Kept for setup</Badge>
												<span class="text-xs text-muted-foreground"
													>{archive.linked_setups?.length
														? archive.linked_setups.map((s) => s.name).join(', ')
														: 'linked to a saved setup'}</span
												>
											{/if}
										</div>
										<div class="grid gap-0.5">
											<span class="text-xs text-muted-foreground md:hidden">Size</span>
											<span class="tabular-nums">{formatBytes(archive.size_bytes)}</span>
											{#if archive.world_bytes !== null}
												<span class="text-xs text-muted-foreground tabular-nums"
													>world {formatBytes(archive.world_bytes)}</span
												>
											{/if}
										</div>
										{#if archive.world_attention.length > 0}
											<div
												class={[
													'grid justify-items-start gap-1 rounded-md border p-3 text-sm md:col-span-full',
													warningBox
												]}
											>
												<span class="font-medium">Attention required</span>
												<ul class="list-disc pl-5">
													{#each archive.world_attention as finding (finding.code)}
														<li>{finding.message}</li>
													{/each}
												</ul>
												<span>
													Check the server log around this time before relying on this backup.
													<a
														class="underline"
														href={resolve('/instances/[id]#console', { id: instance.id })}
														>Open the console</a
													>
												</span>
												{#if canRestore}
													<Button
														variant="outline"
														size="sm"
														disabled={acknowledging !== null}
														onclick={() => acknowledge(archive)}
													>
														{acknowledging === archive.id ? 'Acknowledging…' : 'Acknowledge'}
													</Button>
												{/if}
											</div>
										{/if}
										<div class="flex flex-wrap gap-1 md:col-span-full md:justify-end">
											{#if canCreate && remote?.destination_id}
												<Button
													variant="ghost"
													size="sm"
													disabled={uploading !== null}
													onclick={() => uploadRemote(archive)}
												>
													{uploading === archive.id ? 'Queuing…' : 'Upload now'}
												</Button>
											{/if}
											{#if canDownload}
												<Button
													variant="ghost"
													size="sm"
													href={backups.downloadUrl(instance.id, archive.id)}
													download={archive.filename}
												>
													<Download />
													<span>Download</span>
												</Button>
											{/if}
											{#if canRestore}
												<Button
													variant="ghost"
													size="sm"
													disabled={restoreBlocked !== null}
													title={restoreBlocked ?? undefined}
													onclick={() => askRestore(archive)}
												>
													<History />
													<span>Restore</span>
												</Button>
												<!-- The title sits on a wrapper: a disabled button takes no pointer events. -->
												<span
													title={archive.kept_for_setup
														? 'A saved setup links this backup. Delete the setup first.'
														: undefined}
												>
													<Button
														variant="ghost"
														size="sm"
														disabled={jobRunning || archive.kept_for_setup}
														onclick={() => askDelete(archive)}
													>
														<Trash2 />
														<span>Delete</span>
													</Button>
												</span>
											{/if}
										</div>
									</li>
								{/each}
							</ul>
						</div>
						<Problem error={moreFailure} />
						{#if cursor}
							<Button
								variant="outline"
								class="justify-self-start"
								disabled={loadingMore}
								onclick={loadMore}
							>
								{loadingMore
									? 'Loading…'
									: moreFailure
										? 'Retry loading older backups'
										: 'Load older backups'}
							</Button>
						{/if}
					{/if}
				{/if}
			</Card.Content>
		</Card.Root>
		{#if canList}
			<!-- Apart from the archives: deleting here acts on the live world, not on a backup. -->
			<Card.Root class="min-w-0">
				<Card.Content>
					<WorldsOnDisk {instance} />
				</Card.Content>
			</Card.Root>
		{/if}
	</div>
	{#if (canList && (canSetPolicy || canRestore)) || canSchedule}
		<aside aria-label="Backup settings" class="grid min-w-0 gap-6">
			{#if canList && (canSetPolicy || canRestore)}
				<!--
					The two counts are separate because the classes are: sharing one budget lets a burst
					of hot copies evict every quiesced archive, leaving a full catalogue with nothing
					worth restoring. The list above says which archives the next prune takes, so the
					effect of a change is visible before it deletes anything.
				-->
				<Card.Root>
					<Card.Header><Card.Title>Backup settings</Card.Title></Card.Header>
					<Card.Content class="grid gap-4">
						<div class="grid gap-1">
							<p class="text-sm font-medium">Retention</p>
							<p class="text-sm text-muted-foreground">
								Older backups are deleted after the next backup or scheduled cleanup. Set a count to
								0 to keep all backups of that type. Snapshots taken before a restore, import or
								update are kept apart, up to the same number as consistent backups.
							</p>
						</div>
						<div class="grid gap-3">
							<div class="grid gap-2">
								<Label for="keep-cold">Consistent backups to keep</Label>
								<Input
									id="keep-cold"
									type="number"
									min="0"
									step="1"
									aria-invalid={keepCold === null || !Number.isInteger(keepCold) || keepCold < 0}
									aria-describedby="keep-cold-error"
									disabled={!canRestore}
									bind:value={keepCold}
								/>
								{#if keepCold === null || !Number.isInteger(keepCold) || keepCold < 0}
									<p id="keep-cold-error" class="text-xs text-destructive">
										Enter zero or a positive whole number.
									</p>
								{/if}
							</div>
							<div class="grid gap-2">
								<Label for="keep-hot">Best-effort backups to keep</Label>
								<Input
									id="keep-hot"
									type="number"
									min="0"
									step="1"
									aria-invalid={keepHot === null || !Number.isInteger(keepHot) || keepHot < 0}
									aria-describedby="keep-hot-error"
									disabled={!canRestore}
									bind:value={keepHot}
								/>
								{#if keepHot === null || !Number.isInteger(keepHot) || keepHot < 0}
									<p id="keep-hot-error" class="text-xs text-destructive">
										Enter zero or a positive whole number.
									</p>
								{/if}
							</div>
						</div>
						<div class="flex items-center justify-between gap-4">
							<div class="grid gap-1">
								<Label for="backup-on-restart">Back up on restarts and automatic stops</Label>
								<p class="text-sm text-muted-foreground">
									Restarts, scheduled stops and auto-stops wait for the backup to finish. Large
									worlds can add several minutes. A stop you make by hand does not back up.
								</p>
							</div>
							<Switch id="backup-on-restart" disabled={!canSetPolicy} bind:checked={onRestart} />
						</div>
						<SaveFooter
							saveLabel="Save backup settings"
							changed={policyChanged}
							busy={savingPolicy}
							invalid={!policyValid}
							ondiscard={resetPolicy}
							onsave={savePolicy}
						/>
					</Card.Content>
				</Card.Root>
			{/if}
			{#if canSchedule}
				<p class="text-sm text-muted-foreground">
					Automatic backups, restarts and game updates are set up under
					<a class="underline" href={resolve('/instances/[id]/maintenance', { id: instance.id })}
						>Maintenance</a
					>.
				</p>
			{/if}
		</aside>
	{/if}
</div>

<!--
	A restore replaces the entire world-save directory, so the operator types the world's name
	back. What is there is archived first, and the server is left stopped afterwards.
-->
<WorldImport {instance} />

<WorldImportBackup {instance} />

{#if picked.length === 2}
	<BackupCompare
		instanceId={instance.id}
		pair={[picked[0], picked[1]]}
		{list}
		bind:open={compareOpen}
	/>
{/if}

<RemoteBackupsPanel {instance} {onchange} refreshKey={remoteRefresh} bind:summary={remote} />

<DestructiveConfirm
	bind:open={confirmStop}
	title={`Stop ${instance.name} and back up?`}
	description={`Connected players are disconnected while the server saves the world and stops. It starts again once the backup is written.${consistentRemoval ? ` ${consistentRemoval}` : ''}`}
	confirmLabel="Stop and back up"
	onconfirm={() => void take('quiesced')}
/>
<DestructiveConfirm
	bind:open={restoreOpen}
	name={instance.world_name}
	title="Restore backup for {instance.world_name}?"
	description="The entire world-save directory (worlds_local), including any other worlds in it, is replaced by the archive taken {restoring
		? when(restoring.created_at)
		: ''}. Mods, configuration, server files and player lists are not touched. The panel first backs up what is there now, if anything, as a pre restore backup, and leaves this server stopped afterwards so you can check it before starting."
	confirmLabel="Restore backup"
	onconfirm={restore}
/>

<!--
	F5 again: deleting an archive is not recoverable, so it is named in full and something is
	typed back. What is typed is the server's name, not the archive's own filename: a filename
	carries a timestamp and an id and runs past forty characters, which is friction an operator
	learns to route around rather than read. Which archive is being deleted is settled by the
	row they opened this from and by the description below; the typing is there to stop a
	reflex, and the server is the thing whose data is about to be one archive short.
-->
<DestructiveConfirm
	bind:open={deleteOpen}
	name={instance.name}
	title="Delete this backup?"
	description="{deleting?.filename ?? ''} — taken {deleting
		? when(deleting.created_at)
		: ''}. This backup is permanently deleted now. The current world is not changed."
	confirmLabel="Delete backup"
	onconfirm={remove}
/>
