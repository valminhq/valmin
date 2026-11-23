<script lang="ts">
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import PageHeader from '#lib/components/page-header.svelte';
	import { formatInstant } from '#lib/api/schedules.js';
	import { formatBytes } from '#lib/format.js';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '#lib/api/instances.js';
	import { backups, type Backup } from '#lib/api/backups.js';
	import { ApiError } from '#lib/api/errors.js';
	import { setups, type SavedSetup, type SetupPreview } from '#lib/api/setups.js';
	import type { Job } from '#lib/api/types.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import Problem from '#lib/components/problem.svelte';
	import SetupComparison from '#lib/components/setup-comparison.svelte';
	import SetupBeforeUpdateCard from '#lib/components/setup-before-update-card.svelte';

	const id = $derived(page.params.id ?? '');
	/** May save and delete setups, choose what mod changes save, and roll back settings too. */
	const canManage = $derived(session.allowed(id).includes(actions.setupsManage));
	/** May see the setups and roll back mods and config files. */
	const canSee = $derived(canManage || session.allowed(id).includes(actions.modsManage));
	const canReadConfigs = $derived(session.allowed(id).includes(actions.configRaw));
	const canBackUp = $derived(session.allowed(id).includes(actions.backupsCreate));
	const canRestoreWorld = $derived(session.allowed(id).includes(actions.backupsRestore));
	const canStart = $derived(session.allowed(id).includes(actions.start));
	const canStop = $derived(session.allowed(id).includes(actions.stop));
	/** The world backup choice that takes a backup during the save. */
	const backUpNow = 'now';
	/** The world backup choice that waits for the one the next mod update takes. */
	const linkNext = 'next';

	let instance = $state<Instance | null>(null);
	const stopped = $derived(instance?.state === 'stopped');
	/** A setup saves from, and rolls back, a stopped or a running server. */
	const canSave = $derived(stopped || instance?.state === 'running');
	/** Why the server can neither save a setup nor roll back now. */
	const unsettled = $derived(
		instance?.state === 'error'
			? 'This server needs a check first; open its overview.'
			: 'Wait for this server to finish starting or stopping.'
	);
	let list = $state<SavedSetup[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let actionFailure = $state<unknown>(null);
	let backupFailure = $state<unknown>(null);
	let availableBackups = $state<Backup[]>([]);
	let backupCursor = $state<string | null>(null);
	let loadingBackups = $state(false);
	let name = $state('Working before update');
	let worldBackupId = $state(linkNext);
	let keepPackages = $state(true);
	let selectedId = $state('');
	let preview = $state<SetupPreview | null>(null);
	let previewLoading = $state(false);
	let previewFailure = $state<unknown>(null);
	let restoreOpen = $state(false);
	let deleteOpen = $state(false);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let jobAction = $state<'save' | 'rollback' | 'delete' | null>(null);
	let restoreWorld = $state(false);
	let startAfter = $state(true);
	let restoreSettings = $state(true);
	let jobTarget = $state('');
	let loadRequest = 0;
	let previewRequest = 0;

	const selected = $derived(list.find((setup) => setup.id === selectedId) ?? null);
	const linkedBackups = $derived(availableBackups.filter((backup) => backup.consistent));
	/** The linked world backup, when the rollback can restore it. */
	const worldBackup = $derived(
		preview?.setup.world_backup && preview.setup.world_backup.available !== false
			? preview.setup.world_backup
			: null
	);
	const running = $derived(instance?.state === 'running');
	/** Whether the rollback restores the linked world backup too. */
	const withWorld = $derived(restoreWorld && worldBackup !== null && canRestoreWorld);
	const rollbackDescription = $derived(
		"This replaces the server's managed mods and configuration files" +
			(canManage && restoreSettings ? ', and its launch and backup settings,' : '') +
			' with the saved setup, after saving the current setup as an automatic one, so the rollback can be undone.' +
			(withWorld
				? ' It also replaces the current world with the linked backup, after archiving it. Everything played since that backup is lost.'
				: ' World files are unchanged.') +
			(running ? ' The server is stopped first.' : '') +
			' Valmin rolls the setup back if it fails to apply.'
	);
	const restoreBlocked = $derived.by(() => {
		if (jobRunning) return 'Wait for the current task to finish.';
		if (!canSave) return unsettled;
		if (running && !canStop)
			return "You can't stop this server, so it can't be rolled back while it runs.";
		if (!preview) return 'Load a current preview before rolling back.';
		if (!preview.ready) return 'Resolve the preview problems before rolling back.';
		return null;
	});

	$effect(() => {
		if (canSee) void load(id);
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
				previewFailure = null;
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
			const result = await setups.preview(id, setupId, !(canManage && restoreSettings));
			if (request === previewRequest) preview = result;
		} catch (err) {
			if (request === previewRequest) previewFailure = err;
		} finally {
			if (request === previewRequest) previewLoading = false;
		}
	}

	function compare(setup: SavedSetup) {
		if (setup.id !== selectedId) restoreWorld = false;
		selectedId = setup.id;
		void loadPreview(setup.id);
	}

	async function run(
		action: 'save' | 'rollback' | 'delete',
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

	/** The world backup choice to send: taking one now needs a stopped server, so a running one
	 * waits for the next mod update's instead. */
	const worldChoice = $derived(worldBackupId === backUpNow && !stopped ? linkNext : worldBackupId);

	function save() {
		const value = name.trim();
		if (!value || !canSave || jobRunning) return;
		const choice = worldChoice;
		void run('save', '', () =>
			setups.save(id, value, {
				worldBackupId:
					choice === backUpNow || choice === linkNext ? undefined : choice || undefined,
				backUpWorld: choice === backUpNow,
				linkNextWorld: choice === linkNext,
				light: !keepPackages
			})
		);
	}

	function rollback() {
		const current = preview;
		if (!current || !current.ready || !canSave || jobRunning) return;
		const start = startAfter && canStart && running;
		void run('rollback', current.setup.id, () =>
			setups.rollback(id, current.setup.id, {
				world: withWorld,
				start,
				modsOnly: !(canManage && restoreSettings),
				etag: stopped ? current.etag : undefined
			})
		);
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
		if (jobAction === 'rollback') restoreWorld = false;
		if (jobAction === 'rollback' && jobTarget && jobTarget === selectedId)
			void loadPreview(jobTarget);
		void load(id);
	}

	function when(iso: string): string {
		return formatInstant(iso);
	}
</script>

<div class="grid gap-6">
	<PageHeader title="Saved setups" level={2}>
		Save exact mod versions and every configuration file, then compare and roll back to them later.
		Rolling back, and each mod install, removal and update unless an administrator turned it off,
		saves the current setup first, so the change can be undone. A rollback can restore the world
		backup a setup links in the same step.
	</PageHeader>

	{#if !canSee}
		<p class="text-sm text-muted-foreground">You don't have permission to see saved setups.</p>
	{:else}
		<Problem error={failure} />
		{#if loading && !instance}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if instance}
			{#if canManage}
				<Card.Root>
					<Card.Header>
						<Card.Title>Save current setup</Card.Title>
						<Card.Description>
							Captures launch and backup settings, managed mods, and every file in the configuration
							editor except the console password file. The server may be running or stopped.
							Credentials and world files are not included.
						</Card.Description>
					</Card.Header>
					<Card.Content class="grid gap-4">
						<div class="grid max-w-lg gap-2">
							<Label for="setup-name">Name</Label>
							<Input id="setup-name" bind:value={name} required />
						</div>
						<div class="grid max-w-lg gap-2">
							<Label for="setup-backup">Associated world backup</Label>
							<NativeSelect id="setup-backup" bind:value={worldBackupId}>
								<option value={linkNext}>The backup the next mod update takes</option>
								{#if canBackUp && stopped}
									<option value={backUpNow}>Back up the world now</option>
								{/if}
								<option value="">None</option>
								{#each linkedBackups as backup (backup.id)}
									<option value={backup.id}>{when(backup.created_at)} · {backup.world_name}</option>
								{/each}
							</NativeSelect>
							<p class="text-xs text-muted-foreground">
								{worldChoice === linkNext
									? 'The world backup the next mod update takes is linked, if the mods are still the same then. Installing a new mod, removing one or disabling one before then leaves the setup without one.'
									: 'Only consistent backups from this server can be linked.'}
								A linked backup is kept until the setup is deleted, outside the backup retention counts,
								so each one adds to disk use. Linking does not restore the world.
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
						<div class="grid max-w-lg gap-2">
							<div class="flex items-center gap-2">
								<Switch id="setup-keep-packages" bind:checked={keepPackages} />
								<Label for="setup-keep-packages">Keep package files (restore works offline)</Label>
							</div>
							<p class="text-xs text-muted-foreground">
								{keepPackages
									? 'The setup keeps a copy of every mod package, so a restore needs neither the registry nor the download cache.'
									: 'Light: the setup keeps only mods that cannot be downloaded again, and a restore fetches the rest from the download cache or the registry. It fails for a version the registry has removed.'}
							</p>
						</div>
						<div class="grid justify-items-start gap-2">
							<Button disabled={!canSave || jobRunning || !name.trim()} onclick={save}
								>Save setup</Button
							>
							{#if !canSave}
								<p class="text-sm text-muted-foreground">{unsettled}</p>
							{/if}
						</div>
					</Card.Content>
				</Card.Root>

				<SetupBeforeUpdateCard {instance} onchange={(row) => (instance = row)} />
			{/if}

			<Card.Root>
				<Card.Header>
					<Card.Title>Saved for this server</Card.Title>
					<Card.Description>
						A saved setup never changes. Names can repeat; the date tells them apart.
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
										<span class="text-sm text-muted-foreground"
											>{when(setup.created_at)}{setup.created_by
												? ` · by ${setup.created_by}`
												: ''}</span
										>
										{#if setup.automatic || setup.light || !setup.config_tree}
											<span class="flex flex-wrap gap-1">
												{#if setup.automatic}<Badge variant="outline">Automatic</Badge>{/if}
												{#if setup.light}<Badge variant="outline">Light</Badge>{/if}
												{#if !setup.config_tree}
													<Badge variant="outline">Top-level .cfg only</Badge>
												{/if}
											</span>
										{/if}
										{#if setup.configs_truncated}
											<span class="text-sm text-muted-foreground">
												Some config files are not included: larger than 1 MiB, more than 8 folders
												deep, or past the first 5,000.
											</span>
										{/if}
										{#if setup.world_pending}
											<span class="text-sm">
												World backup: from the next mod update, if the mods are still the same
											</span>
										{/if}
										{#if setup.world_backup}
											<span class="text-sm">
												{setup.world_backup.available === false
													? 'Linked world backup unavailable'
													: `World backup linked: ${when(setup.world_backup.created_at)} · ${setup.world_backup.world_name}${setup.world_backup.size_bytes ? ` · ${formatBytes(setup.world_backup.size_bytes)}` : ''}`}
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
						<SetupComparison
							{preview}
							instanceId={id}
							settingsRestored={canManage && restoreSettings}
							readable={canReadConfigs}
						/>
						<Card.Root>
							<Card.Header>
								<Card.Title>Rollback preview</Card.Title>
								<Card.Description>
									Rolling back applies the saved mods and configuration files, and launch and backup
									settings if chosen, after saving the current setup as an automatic one. A running
									server is stopped first. It does not change the game build, selected world or
									credentials. World files change only if you restore the linked world backup below.
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
												.world_name}. You can also restore it from
											<a class="underline" href={resolve('/instances/[id]/backups', { id })}
												>Backups</a
											>.
										</p>
										{#if canRestoreWorld}
											<div class="flex items-start gap-2">
												<Switch id="rollback-world" bind:checked={restoreWorld} />
												<Label for="rollback-world" class="grid gap-1 font-normal">
													Also restore the linked world backup
													<span class="text-xs text-muted-foreground">
														Replaces the current world with that backup, which is archived first.
														Everything played since it was taken is lost.
													</span>
												</Label>
											</div>
										{/if}
									{/if}
								{:else}
									<p class="text-sm text-muted-foreground">
										No world backup is linked to this setup.
									</p>
								{/if}
								{#if preview.problems.length > 0}
									<div role="alert" class="rounded-md border border-destructive p-3 text-sm">
										<p class="font-medium">This setup is not ready to roll back to</p>
										<ul class="mt-2 list-disc pl-5">
											{#each preview.problems as problem (problem)}<li>{problem}</li>{/each}
										</ul>
									</div>
								{/if}
								{#if canManage}
									<div class="flex items-center gap-2">
										<Switch
											id="rollback-settings"
											bind:checked={restoreSettings}
											onCheckedChange={() => loadPreview(preview?.setup.id ?? selectedId)}
										/>
										<Label for="rollback-settings" class="font-normal">
											Also restore launch and backup settings
										</Label>
									</div>
								{:else}
									<p class="text-sm text-muted-foreground">
										Mods and configuration files are rolled back; launch and backup settings stay as
										they are.
									</p>
								{/if}
								{#if running && canStart}
									<div class="flex items-center gap-2">
										<Switch id="rollback-start" bind:checked={startAfter} />
										<Label for="rollback-start" class="font-normal">
											Start the server again when done
										</Label>
									</div>
								{/if}
								{#if running}
									<p class="text-sm text-muted-foreground">
										This server is running, so it is stopped first and players are disconnected.
									</p>
								{/if}
								<div class="flex flex-wrap items-center gap-3">
									<Button disabled={restoreBlocked !== null} onclick={() => (restoreOpen = true)}
										>Roll back to this setup</Button
									>
									{#if canManage}
										<Button
											variant="destructive"
											disabled={jobRunning}
											onclick={() => (deleteOpen = true)}>Delete setup</Button
										>
									{/if}
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
	name={withWorld ? (instance?.name ?? '') : undefined}
	title="Roll back to this setup?"
	description={rollbackDescription}
	confirmLabel="Roll back"
	onconfirm={rollback}
/>
<DestructiveConfirm
	bind:open={deleteOpen}
	name={instance?.name ?? ''}
	title="Delete this setup?"
	description="{selected?.name ?? ''} · {selected
		? when(selected.created_at)
		: ''}. The saved setup and its retained package copies are released. A linked world backup remains in the backup catalogue and counts toward retention again."
	confirmLabel="Delete setup"
	onconfirm={remove}
/>
