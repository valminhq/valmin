<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import { actions, instances, type CreateInstance, type GameOptions } from '$lib/api/instances';
	import type { Job } from '$lib/api/types';
	import { instanceList } from '$lib/state/instances.svelte';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import * as Select from '$lib/components/ui/select';
	import { Switch } from '$lib/components/ui/switch';
	import { Separator } from '$lib/components/ui/separator';
	import { unsaved } from '$lib/state/dirty.svelte';
	import { tick, untrack } from 'svelte';
	import Field, { focusFirstInvalid } from '$lib/components/field.svelte';
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import ModPicker from '$lib/components/mod-picker.svelte';
	import WorldFilePicker from '$lib/components/world-file-picker.svelte';
	import WorldModifiers from '$lib/components/world-modifiers.svelte';
	import GameBranchPicker from '$lib/components/game-branch-picker.svelte';
	import { mods, type ModSummary } from '$lib/api/mods';
	import { SvelteSet } from 'svelte/reactivity';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let options = $state<GameOptions | null>(null);
	let failure = $state<unknown>(null);
	let busy = $state(false);
	let job = $state<Job | null>(null);
	let importJob = $state<Job | null>(null);

	let name = $state('');
	let serverName = $state('');
	let worldName = $state('');
	let password = $state('');
	let isPublic = $state(false);
	let crossplay = $state(false);
	let preset = $state('');
	let gameBranch = $state('public');
	let gameBranchPassword = $state('');
	let memLimitMB = $state(4096);
	let startAfter = $state(true);
	let modifiers = $state<Record<string, string>>({});
	const modifierCount = $derived(
		Object.values(modifiers).filter((value) => value.trim() !== '').length
	);
	let chosenMods = $state<ModSummary[]>([]);
	let picked = $state<FileList | undefined>();
	let pickedFolder = $state<FileList | undefined>();
	let allowBackupVariant = $state(false);
	let seed = $state('');
	let seedFailure = $state<string | null>(null);

	$effect(() => {
		instances
			.options()
			.then((o) => (options = o))
			.catch((err) => (failure = err));
	});

	// The custom seed package can be chosen directly, by the switch, or arrive with a chosen
	// pack that lists it as a direct dependency. The daemon accepts the same two cases.
	const seedMod = $derived(
		options ? `${options.custom_seed.namespace}-${options.custom_seed.name}` : null
	);
	const seedPacks = new SvelteSet<string>();
	const checkedForSeed = new SvelteSet<string>();
	$effect(() => {
		const want = seedMod;
		const picked = chosenMods;
		if (!want) return;
		untrack(() => {
			for (const m of picked) {
				if (m.full_name === want || checkedForSeed.has(m.full_name)) continue;
				checkedForSeed.add(m.full_name);
				mods
					.detail(m.namespace, m.name, m.source)
					.then((detail) => {
						const latest = detail.versions.find((v) => v.version === m.latest_version);
						if (latest?.dependencies.some((d) => d.startsWith(`${want}-`))) {
							seedPacks.add(m.full_name);
						}
					})
					.catch(() => checkedForSeed.delete(m.full_name));
			}
		});
	});
	const seedDirect = $derived(chosenMods.some((m) => m.full_name === seedMod));
	const seedPack = $derived(chosenMods.find((m) => seedPacks.has(m.full_name)));
	const seedOn = $derived(seedDirect || seedPack !== undefined);

	async function setSeed(on: boolean) {
		if (!options || !seedMod) return;
		seedFailure = null;
		if (!on) {
			chosenMods = chosenMods.filter((m) => m.full_name !== seedMod);
			return;
		}
		const { namespace, name, source } = options.custom_seed;
		try {
			const mod = await mods.detail(namespace, name, source);
			if (!chosenMods.some((m) => m.full_name === seedMod)) chosenMods = [...chosenMods, mod];
		} catch {
			seedFailure = `${seedMod} could not be read from the mod catalogue.`;
		}
	}

	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const minPassword = $derived(options?.min_password_length ?? 5);
	const newInstanceId = $derived(job?.instance_id ?? null);
	/** F3. There is no instance to ask about yet, so the question is the global one: the same
	 * list the create button reads, which carries every action for whoever may create at all. */
	const canImport = $derived(session.allowedGlobally().includes(actions.worldImport));
	// Both pickers feed one upload, as on the settings screen: a 1.0 world is a folder, an
	// older one is a pair of files, and a zip of either is files too (`03 §4`, ADR-180).
	const pickedWorld = $derived([...Array.from(pickedFolder ?? []), ...Array.from(picked ?? [])]);
	const worldFiles = $derived(canImport ? pickedWorld : []);

	// The daemon's own rules on names and passwords come back beside each field; only the
	// minimum length it serves is checked here.
	const localProblems = $derived.by(() => {
		const problems: Record<string, string> = {};
		if (password && password.length < minPassword) {
			problems.password = `At least ${minPassword} characters.`;
		}
		if (options && memLimitMB < options.min_memory_limit_mb) {
			problems.mem_limit_mb = `Use at least ${options.min_memory_limit_mb} MB.`;
		}
		if (options && seedOn) {
			const valid = new RegExp(`^[A-Za-z0-9]{1,${options.custom_seed.max_length}}$`);
			if (seed.trim() !== '' ? !valid.test(seed.trim()) : seedDirect) {
				problems.seed = `Use 1 to ${options.custom_seed.max_length} letters or digits.`;
			}
		}
		return problems;
	});

	function problem(field: string): string | undefined {
		return localProblems[field] ?? apiError?.field(field);
	}

	const ready = $derived(
		name.trim() !== '' &&
			serverName.trim() !== '' &&
			worldName.trim() !== '' &&
			password !== '' &&
			Object.keys(localProblems).length === 0
	);

	// A part-filled form is unsaved work like any editor's. After submission the daemon owns
	// what was entered, with one exception: the world upload is held in this browser and sent
	// once provisioning finishes, so leaving then loses the import rather than a form.
	const started = $derived(
		name.trim() !== '' ||
			serverName.trim() !== '' ||
			worldName.trim() !== '' ||
			password !== '' ||
			chosenMods.length > 0 ||
			picked !== undefined ||
			pickedFolder !== undefined ||
			isPublic ||
			crossplay ||
			preset !== '' ||
			gameBranch !== 'public' ||
			memLimitMB !== 4096 ||
			!startAfter ||
			Object.values(modifiers).some((value) => value.trim() !== '')
	);
	let leaving = $state(false);
	unsaved(() => !leaving && (job === null ? started : worldFiles.length > 0));

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		failure = null;
		const body: CreateInstance = {
			name: name.trim(),
			server_name: serverName.trim(),
			world_name: worldName.trim(),
			password,
			public: isPublic,
			crossplay,
			mem_limit_mb: memLimitMB,
			// An import goes into a stopped server (C19), so a world to bring cancels the
			// chained start and the start is submitted after the import instead.
			start_after_provision: startAfter && worldFiles.length === 0
		};
		if (preset) body.preset = preset;
		if (gameBranch.trim() !== 'public') body.game_branch = gameBranch.trim();
		if (gameBranchPassword) body.game_branch_password = gameBranchPassword;
		const setModifiers = Object.fromEntries(
			Object.entries(modifiers).filter(([, value]) => value.trim() !== '')
		);
		if (Object.keys(setModifiers).length > 0) body.modifiers = setModifiers;
		if (chosenMods.length > 0) {
			// The registry travels with the choice: the same ident can exist on both, and the
			// wizard's picker searches all of them.
			body.mods = chosenMods.map((m) => ({
				full_name: m.full_name,
				version: m.latest_version,
				source: m.source
			}));
		}
		if (seedOn && seed.trim() !== '') body.seed = seed.trim();

		try {
			// 202 and a job, never the instance (`11 §3`, ADR-028): nothing about this server is on
			// disk until the job says so, so the job is what is shown next.
			job = await instances.create(body);
		} catch (err) {
			failure = err;
			// After the rejected fields have rendered, not before.
			void tick().then(() => focusFirstInvalid());
		} finally {
			busy = false;
		}
	}

	/**
	 * The wizard provisions first and imports second, because the endpoint takes an instance
	 * and there is none until the job succeeds (`03 §4.1`). The upload is held in the browser
	 * across the wait rather than staged anywhere, so a provision that fails leaves nothing
	 * behind to clean up.
	 */
	async function finished(finishedJob: Job) {
		if (finishedJob.status !== 'succeeded') return;
		if (worldFiles.length > 0 && newInstanceId) {
			try {
				importJob = await instances.importWorld(newInstanceId, worldFiles, allowBackupVariant);
			} catch (err) {
				failure = err;
			}
			return;
		}
		await done();
	}

	async function imported(finishedJob: Job) {
		// A failed import leaves the progress panel showing why, next to a link to the server
		// it created: the server exists either way, and hiding that behind a redirect makes a
		// world that did not arrive look like one that did (F4).
		if (finishedJob.status !== 'succeeded') return;
		if (startAfter && newInstanceId) {
			try {
				await instances.start(newInstanceId);
			} catch (err) {
				failure = err;
				return;
			}
		}
		await done();
	}

	async function done() {
		leaving = true;
		await instanceList.load();
		await goto(resolve('/'));
	}
</script>

<main class="mx-auto grid max-w-3xl gap-4 p-6">
	<h1 class="text-2xl font-semibold tracking-tight">New server</h1>

	{#if job}
		<Card.Root>
			<Card.Header>
				<Card.Title>Creating {name}</Card.Title>
				<Card.Description>
					The game files are downloaded once and shared between servers, then copied for this one.
					On most filesystems that copy is a real ~1&nbsp;GB copy and takes a while.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<JobProgress jobId={job.job_id} onfinish={finished} />

				<Problem error={failure} />

				{#if importJob}
					<div class="grid gap-2 border-t pt-4">
						<p class="text-sm font-medium">Importing the world</p>
						<p class="text-sm text-muted-foreground">
							The files are uploaded and checked before anything is written, and renamed to
							{worldName} — the world this server starts with.
						</p>
						<JobProgress jobId={importJob.job_id} onfinish={imported} />
						{#if newInstanceId}
							<Button
								variant="outline"
								size="sm"
								class="justify-self-start"
								href={resolve('/instances/[id]', { id: newInstanceId })}
							>
								Go to {name}
							</Button>
						{/if}
					</div>
				{:else if worldFiles.length > 0}
					<p class="text-sm text-muted-foreground" data-testid="import-queued">
						The world is imported once the server exists. Keep this page open until it is done.
					</p>
				{/if}
			</Card.Content>
		</Card.Root>
	{:else}
		<Problem error={failure} />
		<form onsubmit={submit} class="grid gap-4">
			<Card.Root>
				<Card.Header
					><Card.Title>Server details</Card.Title><Card.Description
						>Choose the names and password for this server.</Card.Description
					></Card.Header
				>
				<Card.Content class="grid gap-4 sm:grid-cols-2">
					<Field
						id="name"
						label="Panel name"
						required
						hint="What this server is called in the panel."
						error={problem('name')}
					>
						{#snippet children(field)}
							<Input id="name" bind:value={name} placeholder="Friday Vikings" {...field} />
						{/snippet}
					</Field>

					<Field
						id="server_name"
						label="Server name"
						required
						hint="What players see in the server browser."
						error={problem('server_name')}
					>
						{#snippet children(field)}
							<Input id="server_name" bind:value={serverName} {...field} />
						{/snippet}
					</Field>

					<Field
						id="world_name"
						label="World name"
						required
						hint="The save to create, or the name of an existing world to import."
						error={problem('world_name')}
					>
						{#snippet children(field)}
							<Input id="world_name" bind:value={worldName} {...field} />
						{/snippet}
					</Field>

					<Field
						id="password"
						label="Server password"
						required
						hint="Players need this to join. It must not appear inside the server or world name."
						error={problem('password')}
					>
						{#snippet children(field)}
							<Input
								id="password"
								type="password"
								autocomplete="new-password"
								bind:value={password}
								{...field}
							/>
						{/snippet}
					</Field>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header
					><Card.Title>Player connections</Card.Title><Card.Description
						>Choose how players find and join this server.</Card.Description
					></Card.Header
				>
				<Card.Content class="grid gap-4">
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="public">List publicly</Label>
							<p class="text-sm text-muted-foreground">
								Show this server in the community browser.
							</p>
						</div>
						<Switch id="public" bind:checked={isPublic} />
					</div>

					<Separator />

					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="crossplay">Crossplay</Label>
							<p class="text-sm text-muted-foreground">
								Lets players on other platforms find and join this server.
							</p>
						</div>
						<Switch id="crossplay" bind:checked={crossplay} />
					</div>

					<!--
						`03 §1.4` rule 5, with the list from the daemon so the panel cannot quietly stop
						warning (Q6). The join code belongs to the instance screens, not here.
					-->
					{#if crossplay && options && options.crossplay_untested.length > 0}
						<div class="grid gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3">
							<p class="flex items-center gap-2 text-sm font-medium">
								<TriangleAlert class="size-4" />
								Untested combinations
							</p>
							<p class="text-sm text-muted-foreground">
								Compatibility has not been tested for these combinations:
							</p>
							<ul class="list-inside list-disc text-sm text-muted-foreground">
								{#each options.crossplay_untested as combination (combination)}
									<li>{combination}</li>
								{/each}
							</ul>
							<p class="text-sm text-muted-foreground">
								Turning this off and restarting undoes it; it does not change how worlds are saved.
							</p>
						</div>
					{/if}
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header
					><Card.Title>Gameplay and resources</Card.Title><Card.Description
						>Use the defaults or adjust the game version, world preset, modifiers and memory limit.</Card.Description
					></Card.Header
				>
				<Card.Content class="grid gap-4">
					<GameBranchPicker
						branches={options?.branches ?? []}
						bind:branch={gameBranch}
						bind:password={gameBranchPassword}
					/>
					<div class="grid gap-2">
						<Label for="preset">World preset</Label>
						<Select.Root type="single" bind:value={preset}>
							<Select.Trigger id="preset">
								{preset || 'Server default'}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value="">Server default</Select.Item>
								{#each options?.presets ?? [] as value (value)}
									<Select.Item {value}>{value}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
						{#if options && !options.presets_complete}
							<!--
								The list was built by feeding candidates to the real parser, which confirms
								what it is given and cannot enumerate the rest (`03 §1.3.1`).
							-->
							<p class="text-sm text-muted-foreground">The game may accept other presets too.</p>
						{/if}
					</div>

					<details class="rounded-lg border">
						<summary
							class="cursor-pointer rounded-lg p-3 text-sm font-medium focus-visible:outline-2 focus-visible:outline-ring"
							>World modifiers <span class="ml-2 text-sm font-normal text-muted-foreground"
								>{modifierCount ? `${modifierCount} configured` : 'Optional'}</span
							></summary
						>
						<div class="border-t p-3">
							{#if options}
								<WorldModifiers {options} bind:modifiers />
							{/if}
						</div>
					</details>

					{#if options && seedMod}
						<div class="grid gap-3 rounded-lg border p-3">
							<div class="flex items-center justify-between gap-4">
								<div class="grid gap-1">
									<Label for="custom-seed">Custom seed</Label>
									<p class="text-sm text-muted-foreground">
										{#if seedPack && !seedDirect}
											Included in {seedPack.name}.
										{:else}
											Installs {seedMod} and generates the new world from your seed.
										{/if}
									</p>
								</div>
								<Switch
									id="custom-seed"
									disabled={worldFiles.length > 0 || (seedPack !== undefined && !seedDirect)}
									bind:checked={() => seedOn, (on) => void setSeed(on)}
								/>
							</div>
							{#if seedFailure}
								<p class="text-sm text-destructive">{seedFailure}</p>
							{/if}
							{#if worldFiles.length > 0}
								<p class="text-sm text-muted-foreground">An imported world keeps its own seed.</p>
							{:else if seedOn}
								<Field
									id="seed"
									label="Seed"
									hint={`1 to ${options.custom_seed.max_length} letters or digits. Applies only to a newly generated world.${seedDirect ? '' : " Leave empty to keep the mod's own default."}`}
									error={problem('seed')}
								>
									{#snippet children(field)}
										<Input
											id="seed"
											bind:value={seed}
											maxlength={options?.custom_seed.max_length}
											autocomplete="off"
											{...field}
										/>
									{/snippet}
								</Field>
							{/if}
						</div>
					{/if}

					<Field id="mem" label="Memory limit (MB)" error={problem('mem_limit_mb')}>
						{#snippet children(field)}
							<Input
								id="mem"
								type="number"
								min={options?.min_memory_limit_mb}
								step="256"
								bind:value={memLimitMB}
								{...field}
							/>
						{/snippet}
					</Field>

					{#if options}
						<p class="text-sm text-muted-foreground">
							This server saves every {options.save_defaults.save_interval_seconds / 60} minutes and keeps
							{options.save_defaults.backups} backups of its own, apart from the panel's backups.
						</p>
					{/if}
				</Card.Content>
			</Card.Root>

			<!--
				Chosen here because the world is written on the first boot this wizard can start. The
				daemon installs these between provisioning and that start, so the order on screen is
				the order it happens in.
			-->
			<details class="rounded-xl border bg-card text-card-foreground">
				<summary
					class="cursor-pointer rounded-xl p-5 font-medium focus-visible:outline-2 focus-visible:outline-ring"
					>Mods <span class="ml-2 text-sm font-normal text-muted-foreground"
						>{chosenMods.length ? `${chosenMods.length} selected` : 'Optional'}</span
					></summary
				>
				<div class="grid gap-4 border-t p-5">
					<p class="text-sm text-muted-foreground">
						Install mods before the server first starts, including any that shape a new world.
					</p>
					<ModPicker bind:chosen={chosenMods} />
				</div>
			</details>

			<!--
				`03 §4.1`: offered here and not only as a post-hoc import, because bringing an
				existing world is the first thing a new operator tries.
			-->
			{#if canImport}
				<details class="rounded-xl border bg-card text-card-foreground">
					<summary
						class="cursor-pointer rounded-xl p-5 font-medium focus-visible:outline-2 focus-visible:outline-ring"
						>Start from an existing world <span
							class="ml-2 text-sm font-normal text-muted-foreground"
							>{worldFiles.length ? `${worldFiles.length} files selected` : 'Optional'}</span
						></summary
					>
					<div class="grid gap-4 border-t p-5">
						<p class="text-sm text-muted-foreground">
							Import a save from a single-player game or another server. Without an import, the
							server generates a new world on its first start.
						</p>
						<WorldFilePicker
							bind:picked
							bind:pickedFolder
							bind:allowBackupVariant
							disabled={busy}
						/>
					</div>
				</details>
			{/if}

			<div
				class="flex flex-wrap items-center justify-between gap-4 rounded-xl border bg-card p-5 text-card-foreground"
				data-testid="form-actions"
			>
				<div class="flex items-start gap-3">
					<Switch id="start-after" bind:checked={startAfter} />
					<div class="grid gap-1">
						<Label for="start-after">Start server after setup</Label>
						{#if worldFiles.length > 0}
							<p class="text-sm text-muted-foreground" data-testid="start-after-import">
								A world is imported into a stopped server, so this one starts after the import
								rather than before it.
							</p>
						{/if}
					</div>
				</div>
				<div class="ml-auto flex gap-2">
					<Button variant="outline" href={resolve('/')}>Cancel</Button>
					<Button type="submit" disabled={busy || !ready}>
						{busy ? 'Creating…' : 'Create server'}
					</Button>
				</div>
			</div>
		</form>
	{/if}
</main>
