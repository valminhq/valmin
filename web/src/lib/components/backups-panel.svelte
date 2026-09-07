<script lang="ts">
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
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Download from '@lucide/svelte/icons/download';
	import History from '@lucide/svelte/icons/history';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let list = $state<Backup[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	// The dialog owns its own open flag and writes it back on cancel, so the archive being
	// acted on is tracked beside it rather than inferred from it.
	let restoring = $state<Backup | null>(null);
	let restoreOpen = $state(false);
	let deleting = $state<Backup | null>(null);
	let deleteOpen = $state(false);

	let keepCold = $state(0);
	let keepHot = $state(0);
	let onRestart = $state(false);
	let savingPolicy = $state(false);

	const allowed = $derived(session.allowed(instance.id));
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
		try {
			list = await backups.list(id);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
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

	/**
	 * Saves retention and re-reads the catalogue, because changing a count changes which
	 * archives are marked for the next prune. It writes columns and deletes nothing on its own:
	 * retention is applied by the next backup or prune run (`02 §4.4` step 7).
	 */
	async function savePolicy() {
		savingPolicy = true;
		failure = null;
		try {
			const row = await instances.patch(instance.id, {
				backup_keep_cold: keepCold,
				backup_keep_hot: keepHot,
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

<Card.Root>
	<Card.Header>
		<Card.Title>Backups</Card.Title>
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
							The server is stopped, the world is saved, the archive is taken, and the server is
							started again. It is offline for the whole backup, and this is the archive worth
							restoring from.
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
						<p class="text-sm font-medium">Copy without stopping</p>
						<p class="text-xs text-muted-foreground">
							Best-effort. The world is copied while the server is still writing to it, so the
							archive may hold a half-written save. Use it when downtime is not an option, not when
							the archive has to be good.
						</p>
						<Button
							variant="outline"
							size="sm"
							class="justify-self-start"
							disabled={busy !== null}
							onclick={() => take('hot')}
						>
							Copy while running
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
			{:else if list.length === 0}
				<p class="text-sm text-muted-foreground">
					No archives yet. Nothing has backed this world up.
				</p>
			{:else}
				<div class="overflow-x-auto">
					<table class="w-full text-sm">
						<thead class="text-left text-xs text-muted-foreground">
							<tr>
								<th class="py-2 font-medium">Taken</th>
								<th class="py-2 font-medium">Why</th>
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
												<Badge variant="destructive" title="Outside the retention counts below">
													deleted next prune
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
													<span class="sr-only">Download</span>
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
													<span class="sr-only">Restore</span>
												</Button>
												<Button
													variant="ghost"
													size="sm"
													disabled={jobRunning}
													onclick={() => askDelete(archive)}
												>
													<Trash2 />
													<span class="sr-only">Delete</span>
												</Button>
											{/if}
										</div>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
				{#if restoreBlocked && canRestore}
					<p class="text-sm text-muted-foreground">{restoreBlocked}</p>
				{/if}
			{/if}

			{#if canSetPolicy}
				<!--
					The two counts are separate because the classes are: sharing one budget lets a burst
					of hot copies evict every quiesced archive, leaving a full catalogue with nothing
					worth restoring. The list above says which archives the next prune takes, so the
					effect of a change is visible before it deletes anything.
				-->
				<div class="grid gap-4 border-t pt-4">
					<div class="grid gap-1">
						<p class="text-sm font-medium">Retention</p>
						<p class="text-xs text-muted-foreground">
							Applied after the next backup or scheduled prune, oldest first. Zero keeps everything.
						</p>
					</div>
					<div class="grid gap-3 sm:grid-cols-2">
						<div class="grid gap-2">
							<Label for="keep-cold">Keep the last N full backups</Label>
							<Input id="keep-cold" type="number" min="0" bind:value={keepCold} />
						</div>
						<div class="grid gap-2">
							<Label for="keep-hot">Keep the last N best-effort copies</Label>
							<Input id="keep-hot" type="number" min="0" bind:value={keepHot} />
						</div>
					</div>
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="backup-on-restart">Back up when this server restarts</Label>
							<p class="text-xs text-muted-foreground">
								A restart already stops the server, so the archive costs no extra downtime — but the
								restart waits for it, which on a large world is minutes before the server comes
								back.
							</p>
						</div>
						<Switch id="backup-on-restart" bind:checked={onRestart} />
					</div>
					<Button
						size="sm"
						class="justify-self-start"
						disabled={!policyChanged || savingPolicy}
						onclick={savePolicy}
					>
						{savingPolicy ? 'Saving…' : 'Save retention'}
					</Button>
				</div>
			{/if}
		{/if}
	</Card.Content>
</Card.Root>

<!--
	F5. A restore replaces the world this server loads, so the operator types its name back. The
	panel archives what is there first, which makes the change recoverable rather than undone —
	and the instance is parked afterwards rather than started, because nothing can prove the
	restored world is the one that was wanted (B7).
-->
<DestructiveConfirm
	bind:open={restoreOpen}
	name={instance.world_name}
	title="Restore over {instance.world_name}?"
	description="The world this server loads is replaced by the archive taken {restoring
		? when(restoring.created_at)
		: ''}. The panel archives the world that is there now before it moves anything, and leaves this server stopped afterwards so you can check it before starting."
	confirmLabel="Restore"
	onconfirm={restore}
/>

<!--
	F5 again: deleting an archive is not recoverable, so the archive is named and typed back. It
	is its own filename rather than the world name, because a catalogue holds several archives of
	the same world and only one is being removed.
-->
<DestructiveConfirm
	bind:open={deleteOpen}
	name={deleting?.filename ?? ''}
	title="Delete this archive?"
	description="The archive file and its catalogue entry are both removed. This cannot be undone, and it is not the same as retention — it deletes this one archive now."
	confirmLabel="Delete"
	onconfirm={remove}
/>
