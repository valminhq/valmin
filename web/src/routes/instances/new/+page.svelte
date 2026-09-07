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
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import ModPicker from '$lib/components/mod-picker.svelte';
	import WorldFilePicker from '$lib/components/world-file-picker.svelte';
	import type { ModSummary } from '$lib/api/mods';
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
	let memLimitMB = $state(4096);
	let startAfter = $state(true);
	let modifiers = $state<Record<string, string>>({});
	let showAdvanced = $state(false);
	let chosenMods = $state<ModSummary[]>([]);
	let picked = $state<FileList | undefined>();
	let allowBackupVariant = $state(false);

	$effect(() => {
		instances
			.options()
			.then((o) => (options = o))
			.catch((err) => (failure = err));
	});

	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const minPassword = $derived(options?.min_password_length ?? 5);
	const newInstanceId = $derived(job?.instance_id ?? null);
	/** F3. There is no instance to ask about yet, so the question is the global one: the same
	 * list the create button reads, which carries every action for whoever may create at all. */
	const canImport = $derived(session.allowedGlobally().includes(actions.worldImport));
	const worldFiles = $derived(canImport ? Array.from(picked ?? []) : []);

	// `03 §1.3`'s three rules, client-side as a courtesy: the daemon validates them again, and
	// `08 §5.1` a third time at container creation (G2).
	const localProblems = $derived.by(() => {
		const problems: Record<string, string> = {};
		if (password && password.length < minPassword) {
			problems.password = `At least ${minPassword} characters.`;
		}
		if (password && (serverName.includes(password) || worldName.includes(password))) {
			problems.password = 'The password must not appear inside the server or world name.';
		}
		if (serverName && serverName === worldName) {
			problems.world_name = 'The world name must differ from the server name.';
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
		const setModifiers = Object.fromEntries(
			Object.entries(modifiers).filter(([, value]) => value.trim() !== '')
		);
		if (Object.keys(setModifiers).length > 0) body.modifiers = setModifiers;
		if (chosenMods.length > 0) {
			body.mods = chosenMods.map((m) => ({ full_name: m.full_name, version: m.latest_version }));
		}

		try {
			// 202 and a job, never the instance (`11 §3`, ADR-028): nothing about this server is on
			// disk until the job says so, so the job is what is shown next.
			job = await instances.create(body);
		} catch (err) {
			failure = err;
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
		if (startAfter && newInstanceId) await instances.start(newInstanceId);
		await done();
	}

	async function done() {
		await instanceList.load();
		await goto(resolve('/'));
	}
</script>

<main class="mx-auto grid max-w-2xl gap-4 p-6">
	<h1 class="text-lg font-semibold">New server</h1>

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
						<p class="text-xs text-muted-foreground">
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
				<Card.Content class="grid gap-4">
					<div class="grid gap-2">
						<Label for="name">Panel name</Label>
						<Input id="name" bind:value={name} placeholder="friday-night" />
						<p class="text-xs text-muted-foreground">What this server is called in the panel.</p>
						{#if problem('name')}<p class="text-sm text-destructive">{problem('name')}</p>{/if}
					</div>

					<div class="grid gap-2">
						<Label for="server_name">Server name</Label>
						<Input id="server_name" bind:value={serverName} />
						<p class="text-xs text-muted-foreground">Shown to players in the server browser.</p>
						{#if problem('server_name')}
							<p class="text-sm text-destructive">{problem('server_name')}</p>
						{/if}
					</div>

					<div class="grid gap-2">
						<Label for="world_name">World name</Label>
						<Input id="world_name" bind:value={worldName} />
						{#if problem('world_name')}
							<p class="text-sm text-destructive">{problem('world_name')}</p>
						{/if}
					</div>

					<div class="grid gap-2">
						<Label for="password">Server password</Label>
						<Input
							id="password"
							type="password"
							autocomplete="new-password"
							bind:value={password}
						/>
						{#if problem('password')}
							<p class="text-sm text-destructive">{problem('password')}</p>
						{/if}
					</div>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Content class="grid gap-4">
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="public">List publicly</Label>
							<p class="text-xs text-muted-foreground">
								Show this server in the community browser.
							</p>
						</div>
						<Switch id="public" bind:checked={isPublic} />
					</div>

					<Separator />

					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="crossplay">Crossplay</Label>
							<p class="text-xs text-muted-foreground">
								Lets players on other platforms find and join this server.
							</p>
						</div>
						<Switch id="crossplay" bind:checked={crossplay} />
					</div>

					<!--
						`03 §1.4` rule 5, with the list from the daemon so the panel cannot quietly stop
						warning (Q6). The join code belongs to the instance screens, not here.
					-->
					{#if crossplay && options}
						<div class="grid gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3">
							<p class="flex items-center gap-2 text-sm font-medium">
								<TriangleAlert class="size-4" />
								Untested combinations
							</p>
							<p class="text-sm text-muted-foreground">
								These have never been run, so the panel cannot say whether they work:
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
				<Card.Content class="grid gap-4">
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
							<p class="text-xs text-muted-foreground">
								Measured against build {options.build} by trying each value against the game itself. Other
								presets may exist; the panel does not refuse one it has not seen.
							</p>
						{/if}
					</div>

					<div class="grid gap-2">
						<Label for="mem">Memory limit (MB)</Label>
						<Input id="mem" type="number" min="512" step="256" bind:value={memLimitMB} />
						{#if problem('mem_limit_mb')}
							<p class="text-sm text-destructive">{problem('mem_limit_mb')}</p>
						{/if}
					</div>

					{#if options}
						<p class="text-xs text-muted-foreground">
							This server saves every {options.save_defaults.save_interval_seconds / 60} minutes and keeps
							{options.save_defaults.backups} rolling backups of its own, measured against build
							{options.build}.
						</p>
					{/if}

					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="start-after">Start it once it is ready</Label>
							{#if worldFiles.length > 0}
								<p class="text-xs text-muted-foreground" data-testid="start-after-import">
									A world is imported into a stopped server, so this one starts after the import
									rather than before it.
								</p>
							{/if}
						</div>
						<Switch id="start-after" bind:checked={startAfter} />
					</div>
				</Card.Content>
			</Card.Root>

			<!--
				Chosen here because the world is written on the first boot this wizard can start. The
				daemon installs these between provisioning and that start, so the order on screen is
				the order it happens in.
			-->
			<Card.Root>
				<Card.Header>
					<Card.Title>Mods</Card.Title>
					<Card.Description>
						Optional. These are installed after the game files are in place and before the server
						first starts, so anything that shapes a new world is already on.
					</Card.Description>
				</Card.Header>
				<Card.Content>
					<ModPicker bind:chosen={chosenMods} />
				</Card.Content>
			</Card.Root>

			<!--
				`03 §4.1`: offered here and not only as a post-hoc import, because bringing an
				existing world is the first thing a new operator tries.
			-->
			{#if canImport}
				<Card.Root>
					<Card.Header>
						<Card.Title>Start from an existing world</Card.Title>
						<Card.Description>
							Optional. Bring a save from a single-player game or another server, instead of letting
							this one generate a new world on its first boot.
						</Card.Description>
					</Card.Header>
					<Card.Content class="grid gap-4">
						<WorldFilePicker bind:picked bind:allowBackupVariant disabled={busy} />
					</Card.Content>
				</Card.Root>
			{/if}

			<Card.Root>
				<Card.Content class="grid gap-3">
					<button
						type="button"
						class="text-left text-sm font-medium"
						onclick={() => (showAdvanced = !showAdvanced)}
					>
						World modifiers {showAdvanced ? '−' : '+'}
					</button>
					{#if showAdvanced && options}
						{#if !options.modifier_values_measured}
							<!--
								The five axes are measured (`03 §1.3`); their legal values are not, since the
								`.fwl`'s stored form is not proven to be the command-line grammar (E8). Free
								text rather than an invented dropdown.
							-->
							<p class="flex items-start gap-2 text-xs text-muted-foreground">
								<TriangleAlert class="mt-0.5 size-4 shrink-0" />
								<span>
									The five axes below are measured; their accepted values are not. Leave these blank
									unless you know the value you want.
								</span>
							</p>
						{/if}
						{#each options.modifier_keys as key (key)}
							<div class="grid gap-2">
								<Label for={`modifier-${key}`}>{key}</Label>
								<Input
									id={`modifier-${key}`}
									value={modifiers[key] ?? ''}
									oninput={(event) => (modifiers[key] = event.currentTarget.value)}
								/>
							</div>
						{/each}
					{/if}
				</Card.Content>
			</Card.Root>

			<div class="flex justify-end gap-2">
				<Button variant="outline" href={resolve('/')}>Cancel</Button>
				<Button type="submit" disabled={busy || !ready}>
					{busy ? 'Creating…' : 'Create server'}
				</Button>
			</div>
		</form>
	{/if}
</main>
