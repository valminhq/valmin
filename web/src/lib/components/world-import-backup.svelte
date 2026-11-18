<script lang="ts">
	import { onMount } from 'svelte';
	import { ApiError } from '#lib/api/errors.js';
	import { backups, type Backup, type OrphanedBackup } from '#lib/api/backups.js';
	import { actions, instances, stateSentence, type Instance } from '#lib/api/instances.js';
	import { formatBytes } from '#lib/format.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import Problem from '#lib/components/problem.svelte';

	let { instance }: { instance: Instance } = $props();

	// A source is "live:<id>" for a server that still exists and "gone:<id>" for one whose
	// backups were left behind when it was deleted.
	let servers = $state<Instance[]>([]);
	let leftovers = $state<OrphanedBackup[]>([]);
	let source = $state('');
	let archives = $state<Backup[]>([]);
	let archive = $state('');
	// Set when the daemon says the archive holds several worlds and which of them to take.
	let candidates = $state<string[]>([]);
	let world = $state('');
	let confirming = $state(false);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let failure = $state<unknown>(null);

	const allowed = $derived(session.allowed(instance.id));
	const canImport = $derived(allowed.includes(actions.worldImport));
	const deletedServers = $derived([...new Set(leftovers.map((o) => o.instance_id))]);
	const isDeleted = $derived(source.startsWith('gone:'));
	const sourceId = $derived(source.slice(source.indexOf(':') + 1));
	const choices = $derived(
		isDeleted
			? leftovers
					.filter((o) => o.instance_id === sourceId)
					.map((o) => ({
						value: o.filename,
						label: `${o.filename} · ${formatBytes(o.size_bytes)}`
					}))
			: archives.map((b) => ({
					value: b.id,
					label: `${new Date(b.created_at).toLocaleString()} · ${b.world_name} · ${formatBytes(b.size_bytes)} · ${b.trigger.replaceAll('_', ' ')}`
				}))
	);

	// The same refusal the upload card gives: the daemon only imports into a stopped server.
	const blocked = $derived.by(() => {
		if (jobRunning) return 'An import is running. Wait for it to finish.';
		if (instance.state === 'running') return 'This server is running. Stop it to import a world.';
		if (instance.state !== 'stopped') {
			return `${stateSentence(instance.state)} A world is imported into a stopped server.`;
		}
		return null;
	});
	const ready = $derived(
		canImport && blocked === null && archive !== '' && (candidates.length === 0 || world !== '')
	);

	onMount(() => {
		if (canImport) void loadSources();
	});

	// Servers whose backups this user may download, and, for an administrator, the archives of
	// deleted servers. A failure leaves the lists empty and the card says there is nothing to
	// choose from.
	async function loadSources() {
		try {
			const [all, orphaned] = await Promise.all([
				instances.list(),
				session.allowedGlobally().includes(actions.adopt) ? backups.orphaned() : []
			]);
			servers = all.filter(
				(i) => i.id !== instance.id && session.can(i.id, actions.backupsDownload)
			);
			leftovers = orphaned;
		} catch (err) {
			failure = err;
		}
	}

	// Counts source choices, so a list that arrives after the operator chose again, even the same
	// server again, is dropped. Not state: nothing renders from it.
	let picks = 0;

	// Every page of the server's backups, since an older one is as much a choice as the newest.
	async function chooseSource(value: string) {
		const pick = ++picks;
		source = value;
		archive = '';
		archives = [];
		failure = null;
		forgetWorld();
		if (!value.startsWith('live:')) return;
		try {
			const id = value.slice('live:'.length);
			const all: Backup[] = [];
			let cursor: string | null = null;
			do {
				const page = await backups.list(id, cursor);
				all.push(...page.items);
				cursor = page.next_cursor;
			} while (cursor && pick === picks);
			if (pick === picks) archives = all;
		} catch (err) {
			if (pick === picks) failure = err;
		}
	}

	function forgetWorld() {
		candidates = [];
		world = '';
	}

	async function start() {
		failure = null;
		try {
			const job = await backups.importWorld(instance.id, {
				source_instance_id: sourceId,
				...(isDeleted ? { filename: archive } : { backup_id: archive }),
				...(world ? { world } : {})
			});
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			const named = err instanceof ApiError ? err.details.worlds : undefined;
			if (Array.isArray(named)) {
				candidates = named as string[];
				world = '';
			}
			failure = err;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Import from a backup</Card.Title>
		<Card.Description>
			Take a world from another server's backup, or from a server that was deleted. It replaces the
			world this server loads.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		{#if !canImport}
			<p class="text-sm text-muted-foreground" data-testid="import-backup-blocked">
				Importing a world is not available to you.
			</p>
		{:else if servers.length === 0 && deletedServers.length === 0}
			<p class="text-sm text-muted-foreground" data-testid="import-backup-empty">
				There are no backups of other servers that you can import from.
			</p>
		{:else}
			<div class="grid gap-2">
				<Label for="import-backup-source">Server</Label>
				<NativeSelect
					id="import-backup-source"
					value={source}
					disabled={blocked !== null}
					onchange={(event) => chooseSource(event.currentTarget.value)}
				>
					<option value="">Choose a server</option>
					{#each servers as server (server.id)}
						<option value="live:{server.id}">{server.name}</option>
					{/each}
					{#if deletedServers.length > 0}
						<optgroup label="Deleted servers">
							{#each deletedServers as id (id)}
								<option value="gone:{id}">Deleted server {id}</option>
							{/each}
						</optgroup>
					{/if}
				</NativeSelect>
			</div>

			{#if source !== ''}
				<div class="grid gap-2">
					<Label for="import-backup-archive">Backup</Label>
					<NativeSelect
						id="import-backup-archive"
						bind:value={archive}
						disabled={blocked !== null}
						onchange={forgetWorld}
					>
						<option value="">Choose a backup</option>
						{#each choices as choice (choice.value)}
							<option value={choice.value}>{choice.label}</option>
						{/each}
					</NativeSelect>
				</div>
			{/if}

			{#if candidates.length > 0}
				<div class="grid gap-2">
					<Label for="import-backup-world">World</Label>
					<NativeSelect id="import-backup-world" bind:value={world} disabled={blocked !== null}>
						<option value="">Choose a world</option>
						{#each candidates as name (name)}
							<option value={name}>{name}</option>
						{/each}
					</NativeSelect>
					<p class="text-sm text-muted-foreground">
						This backup holds more than one world. A server loads one.
					</p>
				</div>
			{/if}

			<Problem error={failure} />

			{#if jobId}
				<div class="rounded-lg border p-4">
					<JobProgress {jobId} onfinish={() => (jobRunning = false)} />
				</div>
			{/if}

			<div class="flex flex-wrap items-center justify-between gap-3">
				<p class="text-sm text-muted-foreground">
					{blocked ?? 'The world already here is archived first, so it can be restored.'}
				</p>
				<Button
					variant="destructive"
					size="sm"
					disabled={!ready}
					onclick={() => (confirming = true)}
				>
					Import backup
				</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>

<!--
	Like an upload, this overwrites the world the server loads, so the operator types its name.
-->
<DestructiveConfirm
	bind:open={confirming}
	name={instance.world_name}
	title="Replace {instance.world_name}?"
	description="The world this server loads is replaced by the one in the backup, and the panel archives what is there now before it moves anything — it will be in Backups. The imported files are renamed to {instance.world_name}, because that is the world this server starts with."
	confirmLabel="Import backup"
	onconfirm={start}
/>
