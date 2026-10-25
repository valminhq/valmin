<script lang="ts">
	import { resolve } from '$app/paths';
	import { installExport, type ExportMode, type InstallExport } from '$lib/api/admin';
	import { actions } from '$lib/api/instances';
	import { formatInstant } from '$lib/api/schedules';
	import { formatBytes } from '$lib/conditions';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Download from '@lucide/svelte/icons/download';
	import PackageOpen from '@lucide/svelte/icons/package-open';

	let mode = $state<ExportMode>('light');
	let latest = $state<InstallExport | null>(null);
	let jobId = $state<string | null>(null);
	let running = $state(false);
	let failure = $state<unknown>(null);

	// Rendered from allowed_actions, never from a role name. The daemon answers 404 to a caller
	// without it, so hiding the control matches what the endpoint reports.
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));

	async function load() {
		try {
			latest = (await installExport.latest()).latest;
		} catch (err) {
			failure = err;
		}
	}

	$effect(() => {
		if (allowed) void load();
	});

	async function create() {
		failure = null;
		running = true;
		try {
			jobId = (await installExport.create(mode)).job_id;
		} catch (err) {
			failure = err;
			running = false;
		}
	}

	function finished() {
		running = false;
		void load();
	}
</script>

<main class="mx-auto grid max-w-2xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Export installation</h1>
		<p class="text-sm text-muted-foreground">
			One archive that <code>install.sh --restore</code> turns into this panel on another host. The new
			host downloads the game and the mods again.
		</p>
	</header>

	<Problem error={failure} />

	<Card.Root>
		<Card.Header>
			<Card.Title>New export</Card.Title>
			<Card.Description>
				Jobs on stopped servers, scheduled ones included, are refused until the export finishes;
				cancel it from its job if one is due. Stop the servers first: a running server's world is
				copied as it is at that moment.
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-4">
			<p class="rounded-lg border border-l-4 border-l-destructive bg-muted/40 px-4 py-3 text-sm">
				The archive holds the database and the master key, so anyone who has it can read every
				stored password. Keep it as safe as the host itself.
			</p>

			{#if allowed}
				<fieldset class="grid gap-3" disabled={running}>
					<legend class="sr-only">What to include</legend>
					<label class="flex items-start gap-2 text-sm">
						<input type="radio" name="export-mode" value="light" bind:group={mode} class="mt-1" />
						<span>
							<span class="font-medium">Light</span>
							<span class="block text-muted-foreground">
								Accounts, settings, worlds, mod configs and files mods wrote. Usually tens of
								megabytes. The new host needs Steam and the mod registries, gets the current game
								build of each branch, and starts without local backups.
							</span>
						</span>
					</label>
					<label class="flex items-start gap-2 text-sm">
						<input type="radio" name="export-mode" value="full" bind:group={mode} class="mt-1" />
						<span>
							<span class="font-medium">Full</span>
							<span class="block text-muted-foreground">
								Light, plus the game builds and mod archives in use, the local backups and the saved
								setups. About 1 GB per game build. The servers come back on the exact build they
								ran, even one Steam no longer serves.
							</span>
						</span>
					</label>
				</fieldset>
				<Button class="justify-self-start" onclick={create} disabled={running}>
					<PackageOpen /> Export
				</Button>
			{/if}

			{#if jobId}
				<div class="rounded-lg border p-4">
					<JobProgress {jobId} onfinish={finished} />
				</div>
			{/if}
		</Card.Content>
	</Card.Root>

	{#if latest}
		<Card.Root>
			<Card.Header>
				<Card.Title>Latest export</Card.Title>
				<Card.Description>
					{latest.mode === 'full' ? 'Full' : 'Light'} · {formatBytes(String(latest.size_bytes))} · made
					{formatInstant(latest.created_at)}. A new export replaces it.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<Button variant="outline" href={installExport.downloadUrl} download={latest.filename}>
					<Download /> Download
				</Button>
			</Card.Content>
		</Card.Root>
	{/if}
</main>
