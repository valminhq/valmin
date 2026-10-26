<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { actions, type GameOptions, instances } from '#lib/api/instances.js';
	import {
		manifest,
		templates,
		type CatalogueTemplate,
		type InstanceManifest,
		type ManifestPreview,
		type ManifestSource,
		type TemplateCatalogue
	} from '#lib/api/manifest.js';
	import type { Job } from '#lib/api/types.js';
	import { instanceList } from '#lib/state/instances.svelte.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import Problem from '#lib/components/problem.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
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

	let catalogue = $state<TemplateCatalogue | null>(null);
	let catalogueFailure = $state<unknown>(null);
	let loadingCatalogue = $state(false);
	let filter = $state('');

	const canCreate = $derived(session.allowedGlobally().includes(actions.create));
	const shownTemplates = $derived.by(() => {
		const q = filter.trim().toLowerCase();
		return (catalogue?.templates ?? []).filter(
			(t) => q === '' || `${t.name}\n${t.description}`.toLowerCase().includes(q)
		);
	});
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

	$effect(() => {
		if (canCreate) void loadCatalogue(false);
	});

	async function loadCatalogue(refresh: boolean) {
		loadingCatalogue = true;
		catalogueFailure = null;
		try {
			catalogue = await templates.list(refresh);
		} catch (err) {
			catalogueFailure = err;
		} finally {
			loadingCatalogue = false;
		}
	}

	/** A repository's archive URL shortened to its host and first two path segments, such as
	 * github.com/owner/repo. */
	function repository(url: string): string {
		const u = new URL(url);
		return u.host + u.pathname.split('/').slice(0, 3).join('/');
	}

	async function use(template: CatalogueTemplate) {
		fileName = '';
		fileError = '';
		code = template.code;
		await check({ code: template.code });
	}

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
				'This file could not be read as a template. Choose a template file downloaded from Valmin.';
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

	<h1 class="text-2xl font-semibold tracking-tight">New server from a template</h1>

	{#if !canCreate}
		<p class="text-sm text-muted-foreground">You don't have permission to create servers.</p>
	{:else if job}
		<Card.Root>
			<Card.Header>
				<Card.Title>Creating {name}</Card.Title>
				<Card.Description>
					The game files are downloaded and copied, then each pinned mod is installed in turn, then
					the config from the template is written. Every step is its own job.
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

		{#if catalogue === null || catalogue.sources.length > 0}
			<Card.Root>
				<Card.Header>
					<Card.Title>Shared templates</Card.Title>
					<Card.Description>
						Ready-made servers from template repositories. Choose one to see what it would create.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-3">
					<div class="flex gap-2">
						<Input aria-label="Filter templates" placeholder="Filter" bind:value={filter} />
						<Button
							variant="outline"
							size="sm"
							disabled={loadingCatalogue}
							onclick={() => loadCatalogue(true)}
						>
							<RefreshCw /> Refresh
						</Button>
					</div>
					<Problem error={catalogueFailure} />
					{#each catalogue?.sources ?? [] as source (source.url)}
						{#if source.error}
							<p class="text-sm text-destructive" role="alert">
								{repository(source.url)} could not be read: {source.error}
							</p>
						{/if}
					{/each}
					{#if catalogue && shownTemplates.length === 0}
						<p class="text-sm text-muted-foreground">
							{filter.trim() ? 'No template matches.' : 'No templates yet.'}
						</p>
					{/if}
					{#if shownTemplates.length > 0}
						<ul class="grid gap-2">
							{#each shownTemplates as template (template.source + '/' + template.slug)}
								<li class="flex items-start justify-between gap-3 rounded-md border p-3">
									<div class="grid min-w-0 gap-1">
										<span class="font-medium">{template.name}</span>
										{#if template.description}
											<p class="text-sm whitespace-pre-line text-muted-foreground">
												{template.description}
											</p>
										{/if}
										<span class="text-xs text-muted-foreground">
											{template.mods}
											{template.mods === 1 ? 'mod' : 'mods'} · {repository(template.source)}
										</span>
									</div>
									<Button
										variant="outline"
										size="sm"
										aria-label="Use {template.name}"
										onclick={() => use(template)}
									>
										Use
									</Button>
								</li>
							{/each}
						</ul>
					{/if}
					{#if catalogue && catalogue.problems.length > 0}
						<details class="text-sm text-muted-foreground">
							<summary>
								{catalogue.problems.length}
								{catalogue.problems.length === 1 ? 'template' : 'templates'} could not be read
							</summary>
							<ul class="mt-1 grid gap-0.5">
								{#each catalogue.problems as problem (problem.source + '/' + problem.path)}
									<li>
										<span class="font-mono text-xs"
											>{repository(problem.source)}/{problem.path}</span
										>:
										{problem.detail}
									</li>
								{/each}
							</ul>
						</details>
					{/if}
				</Card.Content>
			</Card.Root>
		{/if}

		<Card.Root>
			<Card.Header>
				<Card.Title>Template file</Card.Title>
				<Card.Description>
					A file downloaded from a server's settings on this panel or another one: launch settings,
					pinned mods and config. It carries no world and no password.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-2">
				<Label for="manifest-file">File</Label>
				<Input id="manifest-file" type="file" accept="application/json,.json" onchange={chose} />
				{#if fileName}
					<p class="text-sm text-muted-foreground">{fileName}</p>
				{/if}
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Or paste a code</Card.Title>
				<Card.Description>
					A template code copied from a server's settings, or a profile code from r2modman or Gale.
					Either brings the mods and their settings.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-2">
				<Label for="template-code">Template code</Label>
				<textarea
					id="template-code"
					rows="4"
					spellcheck="false"
					placeholder="valmin1:… or a profile code"
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
					<Alert.Title>This template cannot be used as it is</Alert.Title>
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
						{#if preview.configs.length > 0 && from && 'code' in from}
							<p class="text-sm text-muted-foreground">
								These settings are applied over the files the mods create.
							</p>
							<ul class="grid gap-2">
								{#each preview.configs as config (config.file)}
									<li class="grid gap-1 rounded-md border p-3">
										<span class="font-mono text-xs">{config.file}</span>
										<ul class="grid gap-0.5 text-xs">
											{#each config.settings ?? [] as setting (setting.key)}
												<li class="flex min-w-0 gap-2">
													<span class="shrink-0 font-mono">{setting.key}</span>
													<span class="truncate text-muted-foreground" title={setting.value}>
														= {setting.value}
													</span>
													{#if setting.secret || setting.suspicious}
														<span class="shrink-0 text-amber-700 dark:text-amber-300">
															looks secret
														</span>
													{/if}
												</li>
											{/each}
										</ul>
									</li>
								{/each}
							</ul>
						{:else if preview.configs.length > 0}
							<p class="text-sm text-muted-foreground">
								{preview.configs.map((c) => c.file).join(', ')}
								— written as they are in the file. A mod's config can hold a key or a webhook, so read
								them if the file came from someone else.
							</p>
						{/if}
					</div>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>New server details</Card.Title>
					<Card.Description>
						Choose a unique panel name and a server password. A template never carries either.
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
						<Label for="start-after">Start server after setup</Label>
						<Switch id="start-after" bind:checked={startAfter} />
					</div>
				</Card.Content>
			</Card.Root>

			<Button class="justify-self-start" disabled={!ready || busy} onclick={submit}>
				{busy ? 'Creating…' : 'Create server'}
			</Button>
		{/if}
	{/if}
</main>
