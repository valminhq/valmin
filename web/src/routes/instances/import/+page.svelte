<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { actions, type GameOptions, instances } from '$lib/api/instances';
	import {
		manifest,
		type InstanceManifest,
		type ManifestPreview,
		type ManifestSource
	} from '$lib/api/manifest';
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
	let from = $state<ManifestSource | null>(null);
	let fileName = $state('');
	let code = $state('');
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
		from !== null &&
			preview !== null &&
			blocking.length === 0 &&
			name.trim() !== '' &&
			password.length >= minPassword
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
		if (!file) return;
		fileName = file.name;
		code = '';
		let parsed: InstanceManifest;
		try {
			parsed = JSON.parse(await file.text()) as InstanceManifest;
		} catch {
			reset();
			fileError =
				'This file could not be read as a server definition. Choose a JSON file exported from Valmin.';
			return;
		}
		await check({ manifest: parsed });
	}

	async function pasted() {
		fileName = '';
		fileError = '';
		await check({ code: code.trim() });
	}

	function reset() {
		from = null;
		preview = null;
		failure = null;
	}

	/** The daemon decides what is wrong with a definition; the page holds no copy of the rules. */
	async function check(next: ManifestSource) {
		reset();
		from = next;
		try {
			preview = await manifest.preview(next);
			if (!name) name = preview.name ?? '';
		} catch (err) {
			failure = err;
		}
	}

	async function submit() {
		if (!from) return;
		busy = true;
		failure = null;
		try {
			job = await manifest.import(from, name.trim(), password, startAfter);
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

		<Card.Root>
			<Card.Header>
				<Card.Title>Or paste a template code</Card.Title>
				<Card.Description>
					A code copied from a server's settings: its mods and the settings changed in the panel.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-2">
				<Label for="template-code">Template code</Label>
				<textarea
					id="template-code"
					rows="4"
					spellcheck="false"
					placeholder="valmin1:…"
					class="w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs break-all outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
					bind:value={code}></textarea>
				<Button
					variant="outline"
					size="sm"
					class="justify-self-start"
					disabled={code.trim() === ''}
					onclick={pasted}
				>
					Check code
				</Button>
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
								{preview.configs.map((c) => c.file).join(', ')}
								{#if from && 'code' in from}
									— these settings are applied over the files the mods create.
								{:else}
									— written as they are in the file. A mod's config can hold a key or a webhook, so
									read them if the file came from someone else.
								{/if}
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
