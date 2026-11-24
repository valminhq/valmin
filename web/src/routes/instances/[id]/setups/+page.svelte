<script lang="ts">
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import PageHeader from '#lib/components/page-header.svelte';
	import { formatInstant } from '#lib/api/schedules.js';
	import { formatBytes } from '#lib/format.js';
	import { backupAge } from '#lib/backup-points.js';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '#lib/api/instances.js';
	import { backups, type Backup } from '#lib/api/backups.js';
	import { ApiError } from '#lib/api/errors.js';
	import { setups, type SavedSetup, type SetupDetail, type SetupPreview } from '#lib/api/setups.js';
	import { configs } from '#lib/api/configs.js';
	import { changeSummary } from '#lib/compare.js';
	import { mods, type InstalledMod } from '#lib/api/mods.js';
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
	import SetupComparison, { type ComparisonSide } from '#lib/components/setup-comparison.svelte';
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
	/** The other side of the comparison: '' for the current server, or a setup's ID. */
	let compareWith = $state('');
	let other = $state<SetupDetail | null>(null);
	/** The server's installed mods, for what changed after the newest setup; null when unread. */
	let currentMods = $state<InstalledMod[] | null>(null);
	let otherFailure = $state<unknown>(null);
	/** Whether the setup the page address names has been opened, which happens once. */
	let linkedOpened = false;
	let previewLoading = $state(false);
	let previewFailure = $state<unknown>(null);
	let restoreOpen = $state(false);
	let deleteOpen = $state(false);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let jobAction = $state<'save' | 'rollback' | 'delete' | null>(null);
	/** The world backup the rollback restores, '' to keep the current world. */
	let rollbackWorldId = $state('');
	/** Whether the rollback lists every world backup rather than the newest few. */
	let allWorlds = $state(false);
	let startAfter = $state(true);
	let restoreSettings = $state(true);
	let jobTarget = $state('');
	let loadRequest = 0;
	let previewRequest = 0;

	const selected = $derived(list.find((setup) => setup.id === selectedId) ?? null);
	/** Backups a setup can keep: consistent ones taken with the mods installed now, or that
	 * recorded no mods. */
	const linkedBackups = $derived(
		availableBackups.filter((backup) => backup.consistent && backup.mods_match_current !== false)
	);
	/** How many world backups the rollback lists before "Show more". */
	const worldsShown = 5;
	/** The backups the rollback can restore: taken with the setup's mods, and the linked one. */
	const worldBackups = $derived(preview?.setup.world_backups ?? []);
	const shownWorlds = $derived(allWorlds ? worldBackups : worldBackups.slice(0, worldsShown));
	/** The chosen world backup, when the rollback can restore it. */
	const chosenWorld = $derived(
		worldBackups.find((backup) => backup.id === rollbackWorldId && backup.available !== false) ??
			null
	);
	const running = $derived(instance?.state === 'running');
	/** Whether the rollback restores a world backup too. */
	const withWorld = $derived(chosenWorld !== null && canRestoreWorld);
	const rollbackDescription = $derived(
		"This replaces the server's managed mods and configuration files" +
			(canManage && restoreSettings ? ', and its launch and backup settings,' : '') +
			' with the saved setup, after saving the current setup as an automatic one, so the rollback can be undone.' +
			(withWorld && chosenWorld
				? ` It also replaces the current world with the backup taken ${when(chosenWorld.created_at)}, after archiving it. Everything played since then is lost.`
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
			const [server, saved, installed] = await Promise.all([
				instances.get(instanceId),
				setups.list(instanceId),
				mods.installed(instanceId).catch(() => null)
			]);
			if (request !== loadRequest) return;
			instance = server;
			list = saved.items;
			currentMods = installed?.mods ?? null;
			const linked = saved.items.find((setup) => setup.id === page.url.searchParams.get('setup'));
			if (linked && !linkedOpened) {
				linkedOpened = true;
				compare(linked);
				rollbackWorldId = page.url.searchParams.get('world') ?? '';
				allWorlds = rollbackWorldId !== '';
			}
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
		if (setup.id !== selectedId) {
			rollbackWorldId = '';
			allWorlds = false;
			compareWith = '';
			other = null;
			otherFailure = null;
		}
		selectedId = setup.id;
		void loadPreview(setup.id);
	}

	/** Loads the setup the selected one is compared with, or clears it for the current server. */
	async function loadOther(setupId: string) {
		other = null;
		otherFailure = null;
		if (!setupId) return;
		try {
			const detail = await setups.get(id, setupId);
			if (compareWith === setupId) other = detail;
		} catch (err) {
			if (compareWith === setupId) otherFailure = err;
		}
	}

	/** A saved setup as one side of the comparison. */
	function setupSide(setup: SetupDetail, label: string): ComparisonSide {
		return {
			label,
			build: setup.game_build_id,
			launch: setup.instance,
			mods: setup.mods,
			configs: setup.configs,
			read: (file) => setups.config(id, setup.id, file)
		};
	}

	/** The pair of sides shown: the current server or another setup, then the selected setup. */
	const sides = $derived.by((): [ComparisonSide, ComparisonSide] | null => {
		if (!preview) return null;
		const chosen = setupSide(preview.setup, 'Saved setup');
		if (compareWith && other)
			return [setupSide(other, `${other.name} (${when(other.created_at)})`), chosen];
		return [
			{
				label: 'Current server',
				build: preview.current.build,
				launch: preview.current.launch,
				mods: preview.current.mods,
				configs: preview.current.configs,
				read: (file) => configs.readRaw(id, file).then((res) => res.text)
			},
			chosen
		];
	});

	/** What changed after each setup: up to the next newer one, or up to the current server for
	 * the newest, whose config files are not compared. */
	const summaries = $derived.by(() => {
		const out: Record<string, string> = {};
		list.forEach((setup, i) => {
			const newer = list[i - 1];
			out[setup.id] = newer
				? changeSummary(setup.mods, newer.mods, setup.configs_digest !== newer.configs_digest)
				: currentMods
					? changeSummary(setup.mods, currentMods)
					: '';
		});
		return out;
	});

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
		const world = withWorld ? chosenWorld?.id : undefined;
		void run('rollback', current.setup.id, () =>
			setups.rollback(id, current.setup.id, {
				worldBackupId: world,
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
		if (jobAction === 'rollback') rollbackWorldId = '';
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
		saves the current setup first, so the change can be undone. A rollback can also restore, in the
		same step, any world backup taken with the setup's mods.
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
							<Label for="setup-backup">World backup to keep with this setup</Label>
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
									: 'Only consistent backups of this server taken with the mods installed now are listed.'}
								A rollback can restore any backup taken with the setup's mods; the one kept here is never
								removed by retention until the setup is deleted, so it adds to disk use. Keeping a backup
								does not restore the world.
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
										{#if summaries[setup.id]}
											<span class="text-sm break-words" data-testid="setup-then"
												>Then: {summaries[setup.id]}</span
											>
										{/if}
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
										{#if setup.world_backups_count && setup.world_backups_newest_at}
											<span class="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
												<span data-testid="setup-worlds"
													>{`Worlds: ${setup.world_backups_count} ${setup.world_backups_count === 1 ? 'backup' : 'backups'} with these mods · newest ${backupAge(setup.world_backups_newest_at, Date.now())}`}</span
												>
												{#if setup.world_backup && setup.world_backup.available !== false}
													<Badge variant="outline">World kept</Badge>
												{/if}
											</span>
										{/if}
										{#if setup.world_backup?.available === false}
											<span class="text-sm text-muted-foreground"
												>Kept world backup unavailable</span
											>
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
					{#if preview && sides}
						<div class="grid max-w-lg gap-2">
							<Label for="setup-compare-with">Compare with</Label>
							<NativeSelect
								id="setup-compare-with"
								bind:value={compareWith}
								onchange={() => loadOther(compareWith)}
							>
								<option value="">Current server</option>
								{#each list.filter((setup) => setup.id !== preview?.setup.id) as setup (setup.id)}
									<option value={setup.id}>{setup.name} · {when(setup.created_at)}</option>
								{/each}
							</NativeSelect>
							<Problem error={otherFailure} />
						</div>
						<SetupComparison
							a={sides[0]}
							b={sides[1]}
							restore={!(compareWith && other)}
							configTree={preview.setup.config_tree && (other?.config_tree ?? true)}
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
									credentials. World files change only if you choose a world backup below.
								</Card.Description>
							</Card.Header>
							<Card.Content class="grid gap-4">
								{#if preview.setup.world_backup?.available === false}
									<p class="text-sm text-destructive">The kept world backup is unavailable.</p>
								{/if}
								{#if worldBackups.length === 0}
									<p class="text-sm text-muted-foreground">
										No backup on disk was taken with exactly these mods, so the world stays as it
										is.
									</p>
								{:else if !canRestoreWorld}
									<p class="text-sm text-muted-foreground">
										{worldBackups.length}
										{worldBackups.length === 1 ? 'backup was' : 'backups were'} taken with these mods.
										Restoring one needs the right to restore backups.
									</p>
								{:else}
									<fieldset class="grid gap-2" data-testid="rollback-world">
										<legend class="mb-1 text-sm font-medium">World</legend>
										<label class="flex items-start gap-3 rounded-md border p-3 text-sm">
											<input
												type="radio"
												name="rollback-world"
												value=""
												class="mt-0.5"
												bind:group={rollbackWorldId}
											/>
											<span class="grid gap-0.5">
												<span>Keep the current world</span>
												<span class="text-xs text-muted-foreground"
													>World files are not touched.</span
												>
											</span>
										</label>
										{#each shownWorlds as world (world.id)}
											<label
												class={[
													'flex items-start gap-3 rounded-md border p-3 text-sm',
													world.available === false && 'opacity-60'
												]}
											>
												<input
													type="radio"
													name="rollback-world"
													value={world.id}
													class="mt-0.5"
													disabled={world.available === false}
													bind:group={rollbackWorldId}
												/>
												<span class="grid gap-1">
													<span
														>Restore the backup from {when(world.created_at)}
														<span class="text-muted-foreground"
															>· {backupAge(world.created_at, Date.now())}</span
														></span
													>
													<span
														class="flex flex-wrap items-center gap-1 text-xs text-muted-foreground"
													>
														{#if world.trigger}{world.trigger.replaceAll('_', ' ')} ·{/if}
														{world.world_name}{#if world.size_bytes}
															· {formatBytes(world.size_bytes)}{/if}
														{#if world.linked}<Badge variant="outline">Kept with this setup</Badge
															>{/if}
														{#if !world.consistent}<Badge variant="secondary">Best-effort</Badge
															>{/if}
														{#if world.available === false}<Badge variant="destructive"
																>Unavailable</Badge
															>{/if}
													</span>
												</span>
											</label>
										{/each}
										{#if worldBackups.length > shownWorlds.length}
											<Button
												variant="ghost"
												size="sm"
												class="justify-self-start"
												onclick={() => (allWorlds = true)}
												>Show {worldBackups.length - shownWorlds.length} more</Button
											>
										{/if}
										<p class="text-xs text-muted-foreground">
											{chosenWorld
												? `Play after ${when(chosenWorld.created_at)} is lost. The current world is archived first.`
												: 'Only backups taken with exactly these mods are listed. Others are on the'}
											{#if !chosenWorld}
												<a class="underline" href={resolve('/instances/[id]/backups', { id })}
													>Backups</a
												> page.
											{/if}
										</p>
									</fieldset>
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
