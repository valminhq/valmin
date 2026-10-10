<script lang="ts">
	import { formatInstant } from '$lib/api/schedules';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { backups, type Backup } from '$lib/api/backups';
	import { ApiError } from '$lib/api/errors';
	import { setups, type SavedSetup, type SetupPreview } from '$lib/api/setups';
	import type { Job } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import SetupComparison from '$lib/components/setup-comparison.svelte';

	const id = $derived(page.params.id ?? '');
	const canManage = $derived(session.allowed(id).includes(actions.setupsManage));

	let instance = $state<Instance | null>(null);
	const stopped = $derived(instance?.state === 'stopped');
	let list = $state<SavedSetup[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let actionFailure = $state<unknown>(null);
	let backupFailure = $state<unknown>(null);
	let availableBackups = $state<Backup[]>([]);
	let backupCursor = $state<string | null>(null);
	let loadingBackups = $state(false);
	let name = $state('Working before update');
	let worldBackupId = $state('');
	let selectedId = $state('');
	let preview = $state<SetupPreview | null>(null);
	let previewLoading = $state(false);
	let previewFailure = $state<unknown>(null);
	let restoreOpen = $state(false);
	let deleteOpen = $state(false);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let jobAction = $state<'save' | 'restore' | 'delete' | null>(null);
	let jobTarget = $state('');
	let loadRequest = 0;
	let previewRequest = 0;

	const selected = $derived(list.find((setup) => setup.id === selectedId) ?? null);
	const linkedBackups = $derived(availableBackups.filter((backup) => backup.consistent));
	const restoreBlocked = $derived.by(() => {
		if (jobRunning) return 'Wait for the current task to finish.';
		if (!stopped) return 'Stop this server to restore a setup.';
		if (!preview) return 'Load a current preview before restoring.';
		if (!preview.ready) return 'Resolve the preview problems before restoring.';
		return null;
	});

	$effect(() => {
		if (canManage) void load(id);
	});

	async function load(instanceId: string) {
		const request = ++loadRequest;
		loading = true;
		failure = null;
		try {
			const [server, saved] = await Promise.all([
				instances.get(instanceId),
				setups.list(instanceId)
			]);
			if (request !== loadRequest) return;
			instance = server;
			list = saved.items;
			if (selectedId && !saved.items.some((setup) => setup.id === selectedId)) {
				selectedId = '';
				preview = null;
			}
		} catch (err) {
			if (request === loadRequest) failure = err;
		} finally {
			if (request === loadRequest) loading = false;
		}
		void loadBackups(instanceId, false);
	}

	async function loadBackups(instanceId: string, older: boolean) {
		if (loadingBackups) return;
		loadingBackups = true;
		backupFailure = null;
		try {
			const page = await backups.list(instanceId, older ? backupCursor : null);
			if (instanceId !== id) return;
			availableBackups = older ? [...availableBackups, ...page.items] : page.items;
			backupCursor = page.next_cursor;
		} catch (err) {
			backupFailure = err;
		} finally {
			loadingBackups = false;
		}
	}

	async function loadPreview(setupId: string) {
		const request = ++previewRequest;
		preview = null;
		previewFailure = null;
		previewLoading = true;
		try {
			const result = await setups.preview(id, setupId);
			if (request === previewRequest) preview = result;
		} catch (err) {
			if (request === previewRequest) previewFailure = err;
		} finally {
			if (request === previewRequest) previewLoading = false;
		}
	}

	function compare(setup: SavedSetup) {
		selectedId = setup.id;
		void loadPreview(setup.id);
	}

	async function run(
		action: 'save' | 'restore' | 'delete',
		target: string,
		call: () => Promise<Job>
	) {
		actionFailure = null;
		try {
			const job = await call();
			jobId = job.job_id;
			jobAction = action;
			jobTarget = target;
			jobRunning = true;
		} catch (err) {
			actionFailure = err;
			if (err instanceof ApiError && err.code === 'stale_write') {
				actionFailure = null;
				preview = null;
				previewFailure = err;
			}
		}
	}

	function save() {
		const value = name.trim();
		if (!value || !stopped || jobRunning) return;
		void run('save', '', () => setups.save(id, value, worldBackupId || undefined));
	}

	function restore() {
		const current = preview;
		if (!current || !current.ready || !stopped || jobRunning) return;
		void run('restore', current.setup.id, () => setups.restore(id, current.setup.id, current.etag));
	}

	function remove() {
		const target = selected;
		if (!target || jobRunning) return;
		void run('delete', target.id, () => setups.remove(id, target.id));
	}

	function finished(job: Job) {
		jobRunning = false;
		if (job.status === 'succeeded' && jobAction === 'delete' && selectedId === jobTarget) {
			selectedId = '';
			preview = null;
		}
		if (jobAction === 'restore') preview = null;
		void load(id);
	}

	function when(iso: string): string {
		return formatInstant(iso);
	}
</script>

<div class="grid gap-6">
	<header class="grid gap-1">
		<h2 class="text-2xl font-semibold tracking-tight">Saved setups</h2>
		<p class="text-sm text-muted-foreground">
			Save exact mod versions and supported configuration, then compare and restore them later.
			World backups are linked for reference and restored separately.
		</p>
	</header>

	{#if !canManage}
		<p class="text-sm text-muted-foreground">Saved setups are available to administrators.</p>
	{:else}
		<Problem error={failure} />
		{#if loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if instance}
			<Card.Root>
				<Card.Header>
					<Card.Title>Save current setup</Card.Title>
					<Card.Description>
						Captures launch and backup settings, managed mods, and supported mod configuration
						files. The server must be stopped. Credentials and world files are not included.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-4">
					<div class="grid max-w-lg gap-2">
						<Label for="setup-name">Name</Label>
						<Input id="setup-name" bind:value={name} required />
					</div>
					<div class="grid max-w-lg gap-2">
						<Label for="setup-backup">Associated world backup (optional)</Label>
						<select
							id="setup-backup"
							class="h-9 rounded-md border border-input bg-background px-3 text-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
							bind:value={worldBackupId}
						>
							<option value="">None</option>
							{#each linkedBackups as backup (backup.id)}
								<option value={backup.id}>{when(backup.created_at)} · {backup.world_name}</option>
							{/each}
						</select>
						<p class="text-xs text-muted-foreground">
							Only consistent backups from this server can be linked. A linked backup is kept until
							the setup is deleted. Linking does not restore the world.
						</p>
						<Problem error={backupFailure} />
						{#if backupCursor}
							<Button
								variant="outline"
								size="sm"
								class="justify-self-start"
								disabled={loadingBackups}
								onclick={() => loadBackups(id, true)}>Load older backups</Button
							>
						{/if}
					</div>
					<div class="grid justify-items-start gap-2">
						<Button disabled={!stopped || jobRunning || !name.trim()} onclick={save}
							>Save setup</Button
						>
						{#if !stopped}
							<p class="text-sm text-muted-foreground">Stop this server to save a setup.</p>
						{/if}
					</div>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Recovery points</Card.Title>
					<Card.Description>
						Saved setups are immutable. Names can repeat; the date identifies each recovery point.
					</Card.Description>
				</Card.Header>
				<Card.Content>
					{#if list.length === 0}
						<p class="text-sm text-muted-foreground">No setups saved for this server.</p>
					{:else}
						<ul class="grid gap-3">
							{#each list as setup (setup.id)}
								<li class="flex flex-wrap items-start justify-between gap-3 rounded-lg border p-4">
									<div class="grid min-w-0 gap-1">
										<strong class="break-words">{setup.name}</strong>
										<span class="text-sm text-muted-foreground">{when(setup.created_at)}</span>
										{#if setup.world_backup}
											<span class="text-sm">
												{setup.world_backup.available === false
													? 'Linked world backup unavailable'
													: `World backup linked: ${when(setup.world_backup.created_at)} · ${setup.world_backup.world_name}`}
											</span>
										{/if}
									</div>
									<Button
										variant={selectedId === setup.id ? 'secondary' : 'outline'}
										size="sm"
										onclick={() => compare(setup)}>Compare and preview</Button
									>
								</li>
							{/each}
						</ul>
					{/if}
				</Card.Content>
			</Card.Root>

			<Problem error={actionFailure} />
			{#if jobId}
				<div class="rounded-lg border p-4"><JobProgress {jobId} onfinish={finished} /></div>
			{/if}

			{#if selected}
				<section class="grid gap-4" aria-label="Setup preview">
					<header class="flex flex-wrap items-start justify-between gap-3">
						<div class="grid gap-1">
							<h3 class="text-xl font-semibold">{selected.name}</h3>
							<p class="text-sm text-muted-foreground">Saved {when(selected.created_at)}</p>
						</div>
						<Button
							variant="outline"
							size="sm"
							disabled={previewLoading}
							onclick={() => loadPreview(selected.id)}>Refresh preview</Button
						>
					</header>
					{#if previewLoading}
						<p class="text-sm text-muted-foreground">Checking setup and current server…</p>
					{/if}
					<Problem error={previewFailure} />
					{#if preview}
						<SetupComparison {preview} />
						<Card.Root>
							<Card.Header>
								<Card.Title>Restore preview</Card.Title>
								<Card.Description>
									Restoring applies the saved mods, supported configuration, and launch and backup
									settings while stopped. It does not change the game build, selected world,
									credentials, or world files. The server stays stopped.
								</Card.Description>
							</Card.Header>
							<Card.Content class="grid gap-4">
								{#if preview.setup.world_backup}
									{#if preview.setup.world_backup.available === false}
										<p class="text-sm text-destructive">The linked world backup is unavailable.</p>
									{:else}
										<p class="text-sm">
											<Badge variant="outline">World backup linked</Badge>
											{when(preview.setup.world_backup.created_at)} · {preview.setup.world_backup
												.world_name}. To restore the world too, choose it separately under
											<a class="underline" href={resolve('/instances/[id]/backups', { id })}
												>Backups</a
											>.
										</p>
									{/if}
								{:else}
									<p class="text-sm text-muted-foreground">
										No world backup is linked to this setup.
									</p>
								{/if}
								{#if preview.problems.length > 0}
									<div role="alert" class="rounded-md border border-destructive p-3 text-sm">
										<p class="font-medium">This setup is not ready to restore</p>
										<ul class="mt-2 list-disc pl-5">
											{#each preview.problems as problem (problem)}<li>{problem}</li>{/each}
										</ul>
									</div>
								{/if}
								<div class="flex flex-wrap items-center gap-3">
									<Button disabled={restoreBlocked !== null} onclick={() => (restoreOpen = true)}
										>Restore setup</Button
									>
									<Button
										variant="destructive"
										disabled={jobRunning}
										onclick={() => (deleteOpen = true)}>Delete setup</Button
									>
								</div>
								{#if restoreBlocked}<p class="text-sm text-muted-foreground">
										{restoreBlocked}
									</p>{/if}
							</Card.Content>
						</Card.Root>
					{/if}
				</section>
			{/if}
		{/if}
	{/if}
</div>

<DestructiveConfirm
	bind:open={restoreOpen}
	name={instance?.name ?? ''}
	title="Restore this setup?"
	description="This replaces the server's managed mods, supported configuration files, and included settings with the saved setup. Valmin rolls these changes back if the restore fails. World files and the selected world are unchanged. The server stays stopped."
	confirmLabel="Restore setup"
	onconfirm={restore}
/>
<DestructiveConfirm
	bind:open={deleteOpen}
	name={instance?.name ?? ''}
	title="Delete this setup?"
	description="{selected?.name ?? ''} · {selected
		? when(selected.created_at)
		: ''}. The saved setup and its retained package copies are released. A linked world backup remains in the backup catalogue."
	confirmLabel="Delete setup"
	onconfirm={remove}
/>
