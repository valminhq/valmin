<script lang="ts">
	import { resolve } from '$app/paths';
	import {
		actions,
		instances,
		isTransient,
		type Instance,
		type UpdateStatus,
		type UpdateTest
	} from '#lib/api/instances.js';
	import { session } from '#lib/state/session.svelte.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { topics } from '#lib/socket/messages.js';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import ChangelogToggle from '#lib/components/changelog-toggle.svelte';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import Problem from '#lib/components/problem.svelte';
	import StateBadge from '#lib/components/state-badge.svelte';
	import FlaskConical from '@lucide/svelte/icons/flask-conical';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let test = $state<UpdateTest | null>(null);
	let status = $state<UpdateStatus | null>(null);
	let jobId = $state<string | null>(null);
	let failure = $state<unknown>(null);
	// Whether the game-build checkbox has its answer, from the first load or from the person.
	// Neither a reload nor a slow first load may overwrite it.
	let chosen = false;
	let tryGame = $state(true);
	let tryMods = $state(true);
	let confirmingApply = $state(false);
	let confirmingDiscard = $state(false);

	// Derived so a replaced row with the same id does not refetch.
	const id = $derived(instance.id);
	const isCopy = $derived(Boolean(instance.test_of));
	// The page replaces the row on every state message; a derived value only changes when the
	// fact does, so the card does not reload or reset its choices each time.
	const modded = $derived(instance.modded);
	const allowed = $derived(session.allowed(id));
	const canTest = $derived(
		[actions.clone, actions.gameUpdate, actions.modsManage].every((a) => allowed.includes(a))
	);
	const busy = $derived(jobId !== null);
	const copyRunning = $derived(test?.copy_state === 'running');
	const copySettled = $derived(test !== null && !isTransient(test.copy_state));
	const sourceSettled = $derived(instance.state === 'stopped' || instance.state === 'running');
	const gameChanged = $derived(test !== null && test.copy_build_id !== test.source_build_id);
	const gameOffered = $derived(status?.update_available !== false);
	// What an apply installs, as the confirmation names it. An apply with nothing to install
	// leaves the server running.
	const applyParts = $derived([
		...(test?.game_newer ? ['the game build'] : []),
		...(test && test.mod_changes.length > 0 ? [`${test.mod_changes.length} mod updates`] : [])
	]);
	const applyDescription = $derived.by(() => {
		const running = instance.state === 'running' && applyParts.length > 0;
		return [
			running ? 'The server is stopped, with its world saved.' : '',
			applyParts.length > 0
				? `The world is backed up, then the copy's changes are installed: ${applyParts.join(' and ')}.`
				: 'The copy runs nothing that differs from this server.',
			running ? 'The server is started again.' : '',
			'The copy is deleted.',
			running ? 'If a step fails, the server stays stopped and the copy is kept.' : ''
		]
			.filter(Boolean)
			.join(' ');
	});

	// Only a modded server can be tested and only someone who can test sees the card, so no one
	// else asks the daemon about it.
	$effect(() => {
		if (isCopy || !modded || !canTest) return;
		void load(id);
	});

	// The copy stops itself when it has been empty for a while, so its state is followed. Derived
	// so a reload that finds the same copy keeps the subscription.
	const copyId = $derived(test?.copy_id);
	$effect(() => {
		if (!copyId) return;
		return socket.subscribe(topics.state(copyId), (m) => {
			if (m.type === 'state') void load(id);
		});
	});

	const testJobs = ['update_test', 'update_apply'];

	/** Reads the copy, the update status and whether a test or apply is still running, so the
	 * card is right after a reload. A failed read keeps what the card showed. */
	async function load(id: string) {
		try {
			const [report, latest, page] = await Promise.all([
				instances.updateTest(id),
				instances.updateStatus(id).catch(() => null),
				instances.jobPage(id, 5)
			]);
			test = report;
			status = latest;
			const running = page.items.find(
				(job) =>
					testJobs.includes(job.kind) && (job.status === 'queued' || job.status === 'running')
			);
			if (running && jobId === null) jobId = running.job_id;
			if (!chosen) {
				tryGame = latest?.update_available !== false;
				chosen = true;
			}
		} catch (err) {
			failure = err;
		}
	}

	/** Runs one request that answers with a job and follows that job. */
	async function run(start: () => Promise<{ job_id: string }>) {
		failure = null;
		try {
			jobId = (await start()).job_id;
		} catch (err) {
			failure = err;
		}
	}

	const startTest = () =>
		run(() => instances.startUpdateTest(id, { game: tryGame, mods: tryMods }));
	const apply = () => run(() => instances.applyUpdateTest(id));
	const discard = () => run(() => instances.remove(test!.copy_id, false));
	const stopCopy = () => run(() => instances.stop(test!.copy_id));

	function finished() {
		jobId = null;
		void load(id);
		onchange?.();
	}

	const limits = (t: UpdateTest) =>
		`${t.copy_mem_limit_mb} MB of memory, ${
			t.copy_cpu_limit === null ? 'all cores' : `${t.copy_cpu_limit} cores`
		}`;
</script>

{#if isCopy}
	<Alert.Root>
		<FlaskConical />
		<Alert.Title>This is a copy made to test an update</Alert.Title>
		<Alert.Description>
			It stops itself after 10 minutes with nobody on it. Apply the update or discard this copy from <a
				class="underline"
				href={resolve('/instances/[id]', { id: instance.test_of ?? '' })}>the original server</a
			>.
		</Alert.Description>
	</Alert.Root>
{:else if modded && canTest}
	<Card.Root id="update-test">
		<Card.Header>
			<Card.Title>Test an update on a copy</Card.Title>
			<Card.Description>
				{#if test}
					Compare {instance.name} with its copy before changing it.
				{:else}
					Copies this server with its latest world and mods, puts the update on the copy, starts it
					and reports which mods failed to load.
				{/if}
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-4">
			{#if busy && jobId}
				<div class="rounded-lg border p-4">
					<JobProgress {jobId} onfinish={finished} />
				</div>
			{:else if test}
				<div class="grid gap-1 text-sm">
					<p class="flex flex-wrap items-center gap-2">
						<a class="font-medium underline" href={resolve('/instances/[id]', { id: test.copy_id })}
							>{test.copy_name}</a
						>
						<StateBadge state={test.copy_state} />
						<span class="text-muted-foreground">
							port {test.copy_port}, {limits(test)}
						</span>
					</p>
					<p>
						Game build
						{#if !test.copy_build_id}
							<span class="text-muted-foreground">not known yet</span>
						{:else if gameChanged}
							<span class="font-mono">{test.source_build_id}</span> becomes
							<span class="font-mono">{test.copy_build_id}</span>
						{:else}
							<span class="font-mono">{test.copy_build_id}</span> is unchanged
						{/if}
					</p>
				</div>

				{#if test.mod_changes.length > 0}
					<div class="grid gap-2">
						<p class="text-sm font-medium">{test.mod_changes.length} mod updates on the copy</p>
						<ul class="grid gap-2">
							{#each test.mod_changes as change (change.full_name)}
								<li class="rounded-md border px-3 py-2 text-sm">
									<span class="font-medium">{change.full_name}</span>
									<span class="text-muted-foreground">
										{change.from ?? 'new'} to {change.to}
									</span>
									<ChangelogToggle
										instanceId={id}
										fullName={change.full_name}
										version={change.to}
										source={change.source}
									/>
								</li>
							{/each}
						</ul>
					</div>
				{/if}

				<div class="grid gap-2 text-sm" data-testid="boot-result">
					{#if test.failed.length > 0}
						<Alert.Root variant="destructive">
							<Alert.Title>
								{test.failed.length}
								{test.failed.length === 1 ? 'mod' : 'mods'} failed to load on the copy
							</Alert.Title>
							<Alert.Description>
								<ul class="grid gap-1">
									{#each test.failed as f (f.full_name)}
										<li>
											<span class="font-medium">{f.full_name}</span>
											<span class="font-mono text-xs break-all">{f.load_error}</span>
										</li>
									{/each}
								</ul>
							</Alert.Description>
						</Alert.Root>
					{:else if test.plugin_load === null}
						<p class="text-muted-foreground">
							The copy has not written a mod loader log, so no mod has been checked yet.
						</p>
					{:else}
						<p>
							{test.plugin_load.loaded}
							{test.plugin_load.loaded === 1 ? 'mod' : 'mods'} loaded on the copy and none failed.
						</p>
					{/if}
					{#if test.not_seen.length > 0}
						<p class="text-muted-foreground">
							The loader said nothing about {test.not_seen.join(', ')}.
						</p>
					{/if}
				</div>

				<div class="flex flex-wrap items-center gap-3">
					<Button
						size="sm"
						disabled={!test.applicable || !sourceSettled}
						onclick={() => (confirmingApply = true)}
					>
						Apply to {instance.name}
					</Button>
					{#if copyRunning}
						<Button size="sm" variant="outline" onclick={stopCopy}>Stop the copy</Button>
					{:else}
						<Button
							size="sm"
							variant="outline"
							disabled={!copySettled}
							onclick={() => (confirmingDiscard = true)}
						>
							Discard the copy
						</Button>
					{/if}
					<span class="text-sm text-muted-foreground">
						{#if copyRunning}
							Join the copy on port {test.copy_port} to try it, and stop it before discarding it.
						{:else}
							Applying stops {instance.name} if it runs, installs this update and starts it again.
						{/if}
					</span>
				</div>
			{:else}
				<div class="grid gap-3 text-sm">
					<p class="text-muted-foreground">
						{instance.name} keeps running and nothing on it changes. The copy runs on half the CPU, is
						not listed publicly, and stops itself after 10 minutes with nobody on it.
					</p>
					<div class="flex items-start gap-2">
						<input
							id="try-game"
							type="checkbox"
							class="mt-0.5 size-4 accent-primary"
							bind:checked={tryGame}
							onchange={() => (chosen = true)}
						/>
						<Label for="try-game" class="grid gap-1 text-sm font-normal">
							Install the new game build
							{#if !gameOffered}
								<span class="text-xs text-muted-foreground">
									This server already has the newest {status?.branch ?? ''} build.
								</span>
							{/if}
						</Label>
					</div>
					<div class="flex items-start gap-2">
						<input
							id="try-mods"
							type="checkbox"
							class="mt-0.5 size-4 accent-primary"
							bind:checked={tryMods}
						/>
						<Label for="try-mods" class="text-sm font-normal">Install the pending mod updates</Label
						>
					</div>
					<div>
						<Button size="sm" disabled={!tryGame && !tryMods} onclick={startTest}>
							Test on a copy
						</Button>
					</div>
				</div>
			{/if}

			<Problem error={failure} />
		</Card.Content>
	</Card.Root>

	{#if test}
		<DestructiveConfirm
			bind:open={confirmingApply}
			name={instance.name}
			title="Apply the update to {instance.name}?"
			description={applyDescription}
			confirmLabel="Apply"
			onconfirm={apply}
		/>
		<DestructiveConfirm
			bind:open={confirmingDiscard}
			name={test.copy_name}
			title="Discard {test.copy_name}?"
			description="The copy is deleted with its world and backups. {instance.name} is not touched."
			confirmLabel="Discard"
			onconfirm={discard}
		/>
	{/if}
{/if}
