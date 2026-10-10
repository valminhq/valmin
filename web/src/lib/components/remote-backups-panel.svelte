<script lang="ts">
	import { formatInstant } from '$lib/api/schedules';
	import { resolve } from '$app/paths';
	import { remoteBackups, type RemoteCopy, type RemoteSummary } from '$lib/api/remote-backups';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { unsaved } from '$lib/state/dirty.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';

	let {
		instance,
		refreshKey = 0,
		onchange
	}: { instance: Instance; refreshKey?: number; onchange?: () => void } = $props();
	const allowed = $derived(session.allowed(instance.id));
	const canList = $derived(allowed.includes(actions.backupsList));
	const canSetPolicy = $derived(allowed.includes(actions.settings));
	const canUpload = $derived(allowed.includes(actions.backupsCreate));
	const canAdmin = $derived(session.allowedGlobally().includes(actions.panelSettings));
	let copies = $state<RemoteCopy[]>([]);
	let summary = $state<RemoteSummary | null>(null);
	const configured = $derived(Boolean(summary?.destination_id));
	let cursor = $state<string | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);
	let saving = $state(false);
	let busy = $state<string | null>(null);
	let enabled = $state(false);
	let cold = $state<number | undefined>(10);
	let hot = $state<number | undefined>(5);
	let snapshots = $state<number | undefined>(10);
	let baseline = $state('');
	const fields = $derived(JSON.stringify([enabled, cold, hot, snapshots]));
	const valid = $derived(
		[cold, hot, snapshots].every((n) => n !== undefined && Number.isInteger(n) && n >= 0)
	);
	unsaved(() => canSetPolicy && fields !== baseline);
	$effect(() => {
		enabled = instance.remote_backup_enabled ?? false;
		cold = instance.remote_keep_cold ?? 10;
		hot = instance.remote_keep_hot ?? 5;
		snapshots = instance.remote_keep_snapshots ?? 10;
		baseline = JSON.stringify([
			instance.remote_backup_enabled ?? false,
			instance.remote_keep_cold ?? 10,
			instance.remote_keep_hot ?? 5,
			instance.remote_keep_snapshots ?? 10
		]);
	});
	$effect(() => {
		const id = instance.id;
		void refreshKey;
		if (!canList) return;
		let disposed = false;
		let fetching = false;
		async function refresh() {
			if (fetching) return;
			fetching = true;
			try {
				const page = await remoteBackups.list(id);
				if (disposed) return;
				summary = page.summary;
				const freshIds = new Set(page.items.map((copy) => copy.id));
				const older = copies.filter((copy) => !freshIds.has(copy.id));
				copies = [...page.items, ...older];
				if (older.length === 0) cursor = page.next_cursor;
				failure = null;
			} catch (err) {
				if (!disposed) failure = err;
			} finally {
				fetching = false;
				if (!disposed) loading = false;
			}
		}
		void refresh();
		const timer = setInterval(() => {
			if (!document.hidden) void refresh();
		}, 5000);
		return () => {
			disposed = true;
			clearInterval(timer);
		};
	});
	async function savePolicy() {
		if (!valid) return;
		saving = true;
		try {
			const row = await instances.patch(instance.id, {
				remote_backup_enabled: enabled,
				remote_keep_cold: cold,
				remote_keep_hot: hot,
				remote_keep_snapshots: snapshots
			});
			enabled = row.remote_backup_enabled ?? false;
			cold = row.remote_keep_cold ?? 10;
			hot = row.remote_keep_hot ?? 5;
			snapshots = row.remote_keep_snapshots ?? 10;
			baseline = JSON.stringify([enabled, cold, hot, snapshots]);
			failure = null;
			onchange?.();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
	async function change(copy: RemoteCopy, operation: 'retry' | 'cancel') {
		busy = copy.id;
		try {
			await remoteBackups[operation](instance.id, copy.id);
			const page = await remoteBackups.list(instance.id);
			copies = page.items;
			cursor = page.next_cursor;
			summary = page.summary;
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}
	async function more() {
		if (!cursor) return;
		busy = 'more';
		try {
			const page = await remoteBackups.list(instance.id, cursor);
			const ids = new Set(copies.map((copy) => copy.id));
			copies = [...copies, ...page.items.filter((copy) => !ids.has(copy.id))];
			cursor = page.next_cursor;
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}
	const when = (value: string | null | undefined) => (value ? formatInstant(value) : 'Never');
	function archiveAge(value: string | null) {
		if (!value) return 'Unknown';
		const hours = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 3600000));
		return hours < 24 ? `${hours} hours` : `${Math.floor(hours / 24)} days`;
	}
	const pending = (copy: RemoteCopy) =>
		['pending', 'uploading', 'retry_wait'].includes(copy.status);
</script>

{#if canList}
	<Card.Root>
		<Card.Header
			><Card.Title>Remote backups</Card.Title><Card.Description
				>Remote copies survive loss of this host. Local and remote retention are independent.</Card.Description
			></Card.Header
		>
		<Card.Content class="grid gap-5">
			<Problem error={failure} />
			{#if loading}<p class="text-sm">Loading remote copies…</p>{/if}
			{#if summary}
				{#if !summary.destination_id}
					<p class="text-sm">
						Remote storage is not configured. {#if canAdmin}<a
								class="underline"
								href={resolve('/admin/remote-backups')}>Configure destination</a
							>{:else}Ask an administrator to configure it.{/if}
					</p>
				{:else}
					<div class="grid gap-2 text-sm sm:grid-cols-2">
						<p>Last successful copy: {when(summary.last_success_at)}</p>
						<p>
							Last copied archive: {when(summary.last_archive_at)} · {archiveAge(
								summary.last_archive_at
							)} old
						</p>
						<p>Latest consistent remote backup: {when(summary.last_consistent_archive_at)}</p>
						<p>{summary.pending} pending · {summary.failed} failed</p>
					</div>
					{#if !summary.enabled}<p class="text-sm">
							Remote storage is disabled. Pending uploads expire after 24 hours.
						</p>{/if}
				{/if}
			{/if}
			{#if canSetPolicy}
				<form
					class="grid gap-4"
					onsubmit={(event) => {
						event.preventDefault();
						void savePolicy();
					}}
				>
					<div class="flex items-center gap-3">
						<Switch id="automatic-remote" disabled={!configured} bind:checked={enabled} /><Label
							for="automatic-remote">Automatically upload new backups</Label
						>
					</div>
					<div class="grid gap-3 sm:grid-cols-3">
						<div class="grid gap-2">
							<Label for="remote-cold">Consistent copies to keep</Label><Input
								id="remote-cold"
								type="number"
								min="0"
								step="1"
								disabled={!configured}
								bind:value={cold}
							/>
						</div>
						<div class="grid gap-2">
							<Label for="remote-hot">Best-effort copies to keep</Label><Input
								id="remote-hot"
								type="number"
								min="0"
								step="1"
								disabled={!configured}
								bind:value={hot}
							/>
						</div>
						<div class="grid gap-2">
							<Label for="remote-snapshots">Safety snapshots to keep</Label><Input
								id="remote-snapshots"
								type="number"
								min="0"
								step="1"
								disabled={!configured}
								bind:value={snapshots}
							/>
						</div>
					</div>
					<p class="text-xs text-muted-foreground">
						0 keeps every copy in that class. Existing local backups are uploaded only when
						requested. Pending archives stay protected locally for up to 24 hours.
					</p>
					<Button
						type="submit"
						class="justify-self-start"
						disabled={!configured || saving || !valid || fields === baseline}
						>{saving ? 'Saving…' : 'Save remote policy'}</Button
					>
				</form>
			{/if}
			{#if configured && copies.length === 0}<p class="text-sm text-muted-foreground">
					No remote copies yet. Use Upload now beside a local backup.
				</p>{/if}
			{#if copies.length > 0}
				<ul class="grid gap-3">
					{#each copies as copy (copy.id)}
						<li class="grid gap-2 rounded-lg border p-3 text-sm sm:grid-cols-[1fr_auto]">
							<div class="grid gap-1">
								<p class="font-medium">{copy.world_name} · {when(copy.archive_created_at)}</p>
								<p>
									{copy.consistent ? 'Consistent' : 'Best-effort'} · {copy.status.replaceAll(
										'_',
										' '
									)} · {copy.attempts} attempts
								</p>
								{#if summary?.destination_id && copy.destination_id !== summary.destination_id}<p
										class="text-xs text-muted-foreground"
									>
										Previous destination
									</p>{/if}
								<p class="text-xs text-muted-foreground">
									{copy.source_available
										? 'Local archive available'
										: 'Local archive no longer available'}
								</p>
								{#if copy.status === 'retry_wait'}<p>
										Next retry: {when(copy.next_attempt_at)}
									</p>{/if}
								{#if copy.last_error}<p class="text-destructive">{copy.last_error}</p>{/if}
								{#if copy.cleanup_pending}<p>Remote cleanup pending. {copy.cleanup_error}</p>{/if}
								{#if copy.job_id && copy.status === 'uploading'}<JobProgress
										jobId={copy.job_id}
									/>{/if}
								{#if copy.job_id}<p class="text-xs text-muted-foreground">
										Attempt job: {copy.job_id}
									</p>{/if}
							</div>
							{#if canUpload}
								<div class="flex items-start gap-2">
									{#if pending(copy)}<Button
											variant="outline"
											size="sm"
											disabled={busy !== null}
											onclick={() => change(copy, 'cancel')}>Cancel upload</Button
										>
									{:else if ['failed', 'cancelled'].includes(copy.status) && copy.source_available}
										<Button
											variant="outline"
											size="sm"
											disabled={busy !== null ||
												!summary?.enabled ||
												copy.destination_id !== summary.destination_id}
											onclick={() => change(copy, 'retry')}>Retry upload</Button
										>
									{/if}
								</div>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
			{#if cursor}<Button
					variant="outline"
					class="justify-self-start"
					disabled={busy !== null}
					onclick={more}>Load older remote copies</Button
				>{/if}
			<p class="text-xs text-muted-foreground">
				To recover on another host, download the archive and manifest, verify SHA-256, extract the
				archive, then use world import.
			</p>
		</Card.Content>
	</Card.Root>
{/if}
