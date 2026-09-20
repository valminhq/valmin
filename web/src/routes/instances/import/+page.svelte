<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { actions, type GameOptions, instances } from '$lib/api/instances';
	import { manifest, type InstanceManifest, type ManifestPreview } from '$lib/api/manifest';
	import type { Job } from '$lib/api/types';
	import { instanceList } from '$lib/state/instances.svelte';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Alert from '$lib/components/ui/alert';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let options = $state<GameOptions | null>(null);
	let failure = $state<unknown>(null);
	let fileError = $state('');
	let doc = $state<InstanceManifest | null>(null);
	let fileName = $state('');
	let preview = $state<ManifestPreview | null>(null);
	let busy = $state(false);
	let job = $state<Job | null>(null);

	let name = $state('');
	let password = $state('');
	let startAfter = $state(false);

	const canCreate = $derived(session.allowedGlobally().includes(actions.create));
	const minPassword = $derived(options?.min_password_length ?? 5);
	const blocking = $derived(preview?.problems ?? []);
	const ready = $derived(
		doc !== null && blocking.length === 0 && name.trim() !== '' && password.length >= minPassword
	);

	$effect(() => {
		instances
			.options()
			.then((o) => (options = o))
			.catch((err) => (failure = err));
	});

	async function chose(event: Event) {
		const file = (event.currentTarget as HTMLInputElement).files?.[0];
		fileError = '';
		doc = null;
		preview = null;
		failure = null;
		if (!file) return;
		fileName = file.name;
		let parsed: InstanceManifest;
		try {
			parsed = JSON.parse(await file.text()) as InstanceManifest;
		} catch {
			fileError =
				'This file could not be read as a server definition. Choose a JSON file exported from Valmin.';
			return;
		}
		doc = parsed;
		if (!name) name = parsed.name ?? '';
		// The daemon decides what is wrong with it. The browser holds no copy of the rules —
		// two validators would drift, and the one nobody notices is the one in the page.
		try {
			preview = await manifest.preview(parsed);
		} catch (err) {
			failure = err;
		}
	}

	async function submit() {
		if (!doc) return;
		busy = true;
		failure = null;
		try {
			job = await manifest.import(doc, name.trim(), password, startAfter);
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	async function finished(finishedJob: Job) {
		if (finishedJob.status !== 'succeeded') return;
		await instanceList.load();
		await goto(resolve('/'));
	}
</script>

<main class="mx-auto grid max-w-2xl gap-4 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<h1 class="text-2xl font-semibold tracking-tight">Import a server definition</h1>

	{#if !canCreate}
		<p class="text-sm text-muted-foreground">
			Creating servers is an administrator capability, so this screen cannot do anything for you.
		</p>
	{:else if job}
		<Card.Root>
			<Card.Header>
				<Card.Title>Creating {name}</Card.Title>
				<Card.Description>
					The game files are downloaded and copied, then each pinned mod is installed in turn, then
					the config from the file is written. Every step is its own job.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<JobProgress jobId={job.job_id} onfinish={finished} />
				<Problem error={failure} />
				{#if fileError}<p class="text-sm text-destructive" role="alert">{fileError}</p>{/if}
			</Card.Content>
		</Card.Root>
	{:else}
		<Problem error={failure} />
		{#if fileError}<p class="text-sm text-destructive" role="alert">{fileError}</p>{/if}

		<Card.Root>
			<Card.Header>
				<Card.Title>The file</Card.Title>
				<Card.Description>
					A definition downloaded from this panel or another one: launch settings, pinned mods and
					config. It carries no world and no password.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-2">
				<Label for="manifest-file">Manifest</Label>
				<Input id="manifest-file" type="file" accept="application/json,.json" onchange={chose} />
				{#if fileName}
					<p class="text-sm text-muted-foreground">{fileName}</p>
				{/if}
			</Card.Content>
		</Card.Root>

		{#if preview}
			{#if blocking.length > 0}
				<Alert.Root variant="destructive">
					<TriangleAlert />
					<Alert.Title>This file cannot be imported as it is</Alert.Title>
					<Alert.Description class="grid gap-1">
						{#each blocking as problem, i (i)}
							<span>{problem.detail}</span>
						{/each}
					</Alert.Description>
				</Alert.Root>
			{/if}

			<Card.Root>
				<Card.Header>
					<Card.Title>What this would create</Card.Title>
				</Card.Header>
				<Card.Content class="grid gap-3 text-sm">
					<div class="grid gap-1">
						<span class="text-muted-foreground">Server</span>
						<span>
							{preview.instance.server_name} · world {preview.instance.world_name} ·
							{preview.instance.mem_limit_mb} MB
							{preview.instance.crossplay ? '· crossplay' : ''}
							{preview.instance.public ? '· public' : ''}
						</span>
					</div>

					<div class="grid gap-1">
						<span class="text-muted-foreground">
							{preview.mods.length}
							{preview.mods.length === 1 ? 'mod' : 'mods'}, pinned
						</span>
						{#if preview.mods.length > 0}
							<ul class="grid gap-0.5">
								{#each preview.mods as mod (mod.full_name)}
									<li class="flex flex-wrap items-center gap-2">
										<span class="font-mono text-xs">{mod.full_name}</span>
										<span class="text-xs text-muted-foreground">{mod.version}</span>
										{#if !mod.available}
											<span class="text-xs text-destructive">not in the catalogue</span>
										{/if}
									</li>
								{/each}
							</ul>
						{/if}
					</div>

					<div class="grid gap-1">
						<span class="text-muted-foreground">
							{preview.configs.length}
							{preview.configs.length === 1 ? 'config file' : 'config files'}
						</span>
						{#if preview.configs.length > 0}
							<p class="text-sm text-muted-foreground">
								{preview.configs.map((c) => c.file).join(', ')} — written as they are in the file. A mod's
								config can hold a key or a webhook, so read them if the file came from someone else.
							</p>
						{/if}
					</div>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>New server details</Card.Title>
					<Card.Description>
						Choose a unique panel name and a server password. These are not included in the
						definition file.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-4">
					<div class="grid gap-2">
						<Label for="name">Name</Label>
						<Input id="name" bind:value={name} autocomplete="off" />
					</div>
					<div class="grid gap-2">
						<Label for="password">Server password</Label>
						<Input
							id="password"
							type="password"
							bind:value={password}
							autocomplete="new-password"
						/>
						<p class="text-sm text-muted-foreground">At least {minPassword} characters.</p>
					</div>
					<div class="flex items-center justify-between gap-3">
						<Label for="start-after">Start server after import</Label>
						<Switch id="start-after" bind:checked={startAfter} />
					</div>
				</Card.Content>
			</Card.Root>

			<Button class="justify-self-start" disabled={!ready || busy} onclick={submit}>
				{busy ? 'Importing…' : 'Import server definition'}
			</Button>
		{/if}
	{/if}
</main>
