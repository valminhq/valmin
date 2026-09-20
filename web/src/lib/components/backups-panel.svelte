<script lang="ts">
	import SchedulesEditor from '$lib/components/schedules-editor.svelte';
	import { scheduleKinds } from '$lib/api/schedules';
	import { backups, type Backup, type BackupMode } from '$lib/api/backups';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import WorldsOnDisk from '$lib/components/worlds-on-disk.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Download from '@lucide/svelte/icons/download';
	import History from '@lucide/svelte/icons/history';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let list = $state<Backup[]>([]);
	let cursor = $state<string | null>(null);
	let loadingMore = $state(false);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let loadFailure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	// The dialog owns its own open flag and writes it back on cancel, so the archive being
	// acted on is tracked beside it rather than inferred from it.
	let restoring = $state<Backup | null>(null);
	let restoreOpen = $state(false);
	let deleting = $state<Backup | null>(null);
	let deleteOpen = $state(false);

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
		void load(instance.id);
	});

	/** Takes the daemon's row as the retention form's baseline, so the screen never shows a
	 * count it merely sent (F4). Re-runs when the parent re-reads the row after a save. */
	$effect(() => {
		keepCold = instance.backup_keep_cold;
		keepHot = instance.backup_keep_hot;
		onRestart = instance.backup_on_restart;
	});

	async function load(id: string) {
		loading = true;
		loadFailure = null;
		try {
			const page = await backups.list(id);
			list = page.items;
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
		loadFailure = null;
		try {
			const page = await backups.list(instance.id, cursor);
			list = [...list, ...page.items];
			cursor = page.next_cursor;
		} catch (err) {
			loadFailure = err;
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

	async function run(call: () => Promise<{ job_id: string }>) {
		failure = null;
		try {
			const job = await call();
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			failure = err;
		}
	}

	const take = (mode: BackupMode) => run(() => backups.create(instance.id, mode));

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

	const policyChanged = $derived(
		keepCold !== instance.backup_keep_cold ||
			keepHot !== instance.backup_keep_hot ||
			onRestart !== instance.backup_on_restart
	);
	const policyValid = $derived(
		keepCold !== null &&
			Number.isInteger(keepCold) &&
			keepCold >= 0 &&
			keepHot !== null &&
			Number.isInteger(keepHot) &&
			keepHot >= 0
	);

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
			const row = await instances.patch(instance.id, {
				backup_keep_cold: cold,
				backup_keep_hot: hot,
				backup_on_restart: onRestart
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
		if (jobRunning) return 'A job is running on this server. Wait for it to finish.';
		if (instance.state !== 'running' && instance.state !== 'stopped') {
			return `This server is ${instance.state.replaceAll('_', ' ')}.`;
		}
		return null;
	});
	const restoreBlocked = $derived.by(() => {
		if (busy) return busy;
		if (instance.state !== 'stopped') return 'Stop this server to restore a world into it.';
		return null;
	});

	function bytes(n: number): string {
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		let v = n;
		let u = 0;
		while (v >= 1024 && u < units.length - 1) {
			v /= 1024;
			u += 1;
		}
		return `${v.toFixed(u === 0 ? 0 : 1)} ${units[u]}`;
	}

	function when(iso: string): string {
		return new Date(iso).toLocaleString();
	}
</script>

<div
	class="grid items-start gap-6 {(canList && canSetPolicy) || canSchedule
		? 'xl:grid-cols-[minmax(0,1fr)_22rem]'
		: ''}"
>
	<Card.Root class="min-w-0">
		<Card.Header>
			<Card.Title>Backup history</Card.Title>
			<Card.Description>
				Archives of this server's world, newest first. Restoring one replaces the world this server
				loads.
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-4">
			{#if !canList}
				<p class="text-sm text-muted-foreground" data-testid="backups-blocked">
					Backups are not available to you.
				</p>
			{:else}
				<WorldsOnDisk {instance} />
				{#if canCreate}
					<!--
					B12. The two controls are not two speeds of the same thing, and the copy is the only
					place that difference is visible. A quiesced archive stops the server, which is what
					makes it trustworthy; a hot copy reads a world that is being written to, so it is
					best-effort and never the recommended one.
				-->
					<div class="grid gap-3 sm:grid-cols-2">
						<div class="grid gap-2 rounded-lg border p-4">
							<p class="text-sm font-medium">Back up now</p>
							<p class="text-xs text-muted-foreground">
								If the server is running, Valmin stops it and waits for the world to finish saving
								before creating a backup, then starts it again. The server is offline for the whole
								backup. A stopped server stays stopped.
							</p>
							<Button
								size="sm"
								class="justify-self-start"
								disabled={busy !== null}
								onclick={() => take('quiesced')}
							>
								Stop and back up
							</Button>
						</div>
						<div class="grid gap-2 rounded-lg border p-4">
							<p class="text-sm font-medium">Back up without stopping</p>
							<p class="text-xs text-muted-foreground">
								Best-effort backup without stopping the server. If the server is running, the backup
								may contain an incomplete save and may not be restorable. Use Stop and back up when
								you can allow downtime.
							</p>
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
				{:else if list.length === 0}
					<p class="text-sm text-muted-foreground">No backups are available for this server.</p>
				{:else}
					<div class="overflow-x-auto">
						<table class="w-full text-sm">
							<thead class="text-left text-xs text-muted-foreground">
								<tr>
									<th class="py-2 font-medium">Created</th>
									<th class="py-2 font-medium">Reason</th>
									<th class="py-2 font-medium">Size</th>
									<th class="py-2"><span class="sr-only">Actions</span></th>
								</tr>
							</thead>
							<tbody>
								{#each list as archive (archive.id)}
									<tr class="border-t">
										<td class="py-2 align-top">
											<div>{when(archive.created_at)}</div>
											<div class="font-mono text-xs text-muted-foreground">
												{archive.world_name}
											</div>
										</td>
										<td class="py-2 align-top">
											<div class="flex flex-wrap items-center gap-1">
												<Badge variant="outline">{archive.trigger.replaceAll('_', ' ')}</Badge>
												{#if !archive.consistent}
													<!-- B12: a hot copy says so wherever it is listed, not only where it
													was taken. -->
													<Badge variant="secondary" title="Copied while the server was running">
														best-effort
													</Badge>
												{/if}
												{#if archive.prunes_next}
													<!-- A retention setting whose effect is invisible until it deletes
													something is the wrong shape for world data. -->
													<Badge
														variant="destructive"
														title="Will be deleted when the backup retention policy next runs"
													>
														Pending deletion
													</Badge>
												{/if}
											</div>
										</td>
										<td class="py-2 align-top tabular-nums">{bytes(archive.size_bytes)}</td>
										<td class="py-2 align-top">
											<div class="flex flex-wrap justify-end gap-1">
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
														onclick={() => askRestore(archive)}
													>
														<History />
														<span>Restore</span>
													</Button>
													<Button
														variant="ghost"
														size="sm"
														disabled={jobRunning}
														onclick={() => askDelete(archive)}
													>
														<Trash2 />
														<span>Delete</span>
													</Button>
												{/if}
											</div>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
					{#if cursor}
						<Button
							variant="outline"
							class="justify-self-start"
							disabled={loadingMore}
							onclick={loadMore}
						>
							{loadingMore ? 'Loading…' : 'Load older backups'}
						</Button>
					{/if}
					{#if restoreBlocked && canRestore}
						<p class="text-sm text-muted-foreground">{restoreBlocked}</p>
					{/if}
				{/if}
			{/if}
		</Card.Content>
	</Card.Root>
	{#if (canList && canSetPolicy) || canSchedule}
		<aside aria-label="Backup settings and schedules" class="grid min-w-0 gap-6">
			{#if canList && canSetPolicy}
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
							<p class="text-xs text-muted-foreground">
								Older backups are deleted after the next backup or scheduled cleanup. Set a count to
								0 to keep all backups of that type.
							</p>
						</div>
						<div class="grid gap-3">
							<div class="grid gap-2">
								<Label for="keep-cold">Backups to keep (server stopped)</Label>
								<Input
									id="keep-cold"
									type="number"
									min="0"
									step="1"
									aria-invalid={keepCold === null || !Number.isInteger(keepCold) || keepCold < 0}
									aria-describedby="keep-cold-error"
									bind:value={keepCold}
								/>
								{#if keepCold === null || !Number.isInteger(keepCold) || keepCold < 0}
									<p id="keep-cold-error" class="text-xs text-destructive">
										Enter zero or a positive whole number.
									</p>
								{/if}
							</div>
							<div class="grid gap-2">
								<Label for="keep-hot">Backups to keep (best-effort)</Label>
								<Input
									id="keep-hot"
									type="number"
									min="0"
									step="1"
									aria-invalid={keepHot === null || !Number.isInteger(keepHot) || keepHot < 0}
									aria-describedby="keep-hot-error"
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
								<Label for="backup-on-restart">Back up when this server restarts</Label>
								<p class="text-xs text-muted-foreground">
									The restart waits for the backup to finish before starting the server again. Large
									worlds can add several minutes of downtime.
								</p>
							</div>
							<Switch id="backup-on-restart" bind:checked={onRestart} />
						</div>
						<Button
							size="sm"
							class="justify-self-start"
							disabled={!policyValid || !policyChanged || savingPolicy}
							onclick={savePolicy}
						>
							{savingPolicy ? 'Saving…' : 'Save backup settings'}
						</Button>
					</Card.Content>
				</Card.Root>
			{/if}
			{#if canSchedule}<SchedulesEditor {instance} />{/if}
		</aside>
	{/if}
</div>

<!--
	F5. A restore replaces the world this server loads, so the operator types its name back. The
	panel archives what is there first, which makes the change recoverable rather than undone —
	and the instance is parked afterwards rather than started, because nothing can prove the
	restored world is the one that was wanted (B7).
-->
<DestructiveConfirm
	bind:open={restoreOpen}
	name={instance.world_name}
	title="Restore backup for {instance.world_name}?"
	description="The world this server loads is replaced by the archive taken {restoring
		? when(restoring.created_at)
		: ''}. The panel archives the world that is there now before it moves anything, and leaves this server stopped afterwards so you can check it before starting."
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
