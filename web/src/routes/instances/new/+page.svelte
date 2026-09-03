<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import { instances, type CreateInstance, type GameOptions } from '$lib/api/instances';
	import type { Job } from '$lib/api/types';
	import { instanceList } from '$lib/state/instances.svelte';
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
	import type { ModSummary } from '$lib/api/mods';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let options = $state<GameOptions | null>(null);
	let failure = $state<unknown>(null);
	let busy = $state(false);
	let job = $state<Job | null>(null);

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

	$effect(() => {
		instances
			.options()
			.then((o) => (options = o))
			.catch((err) => (failure = err));
	});

	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const minPassword = $derived(options?.min_password_length ?? 5);

	// 03 §1.3's three rules, client-side as a courtesy only. The server validates them
	// again (and 08 §5.1 a third time at container creation, G2) — these three are the cause
	// of most "server won't boot" reports, and catching them here saves a round trip, not a
	// check.
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
			start_after_provision: startAfter
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
			// 202 and a job, never the instance (11 §3, ADR-028). Nothing about this server
			// is real on disk until the job says so, which is why the next thing on screen is
			// the job and not a row in the list.
			job = await instances.create(body);
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
			<Card.Content>
				<JobProgress jobId={job.job_id} onfinish={finished} />
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
						03 §1.4 rule 5, and the list comes from the daemon so the panel cannot
						quietly stop saying it. Q6 blocks advertising crossplay as supported.
						Nothing here promises the field Q25 is still looking for: it was
						measured empty, and until someone finds where it appears the panel
						says nothing about it.
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
								03 §1.3.1: the list was enumerated by feeding candidates to the real
								parser, which can confirm what it is given and cannot enumerate what nobody
								tried. Saying so is the difference between a measurement and a claim.
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
						<Label for="start-after">Start it once it is ready</Label>
						<Switch id="start-after" bind:checked={startAfter} />
					</div>
				</Card.Content>
			</Card.Root>

			<!--
				Mods are chosen here rather than only after the server exists, and the ordering
				above is the reason: this wizard can start the server itself, and the world is
				written on that first boot. A mod added afterwards arrives after the thing it may
				have had something to say about — and adding it means stopping the server this page
				just started. The daemon installs these between provisioning and the start, so the
				order on screen is the order it happens in.
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
								E8. The five axes are measured (03 §1.3); their legal values are not.
								The only evidence is a `.fwl`'s stored `combat_default:…` form, which 03 §4.2
								says in the same breath is not proven to be the command-line grammar — so
								there is no dropdown here, because inventing one would present inference as
								measurement and the operator would find out when the server refused to boot.
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
