<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import {
		actions,
		adoption,
		instances,
		type AdoptInstance,
		type AdoptionPreview,
		type GameOptions
	} from '$lib/api/instances';
	import type { Job } from '$lib/api/types';
	import { ApiError } from '$lib/api/errors';
	import { instanceList } from '$lib/state/instances.svelte';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Alert from '$lib/components/ui/alert';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import ShieldCheck from '@lucide/svelte/icons/shield-check';

	const containerID = $derived(page.params.container_id ?? '');
	const canAdopt = $derived(session.allowedGlobally().includes(actions.adopt));

	let preview = $state<AdoptionPreview | null>(null);
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
	let modifiers = $state<Record<string, string>>({});
	let extraArgs = $state('');
	let memLimitMB = $state(4096);
	let cpuLimit = $state('');
	let advanced = $state(false);

	const minPassword = $derived(options?.min_password_length ?? 5);
	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const parsedCPULimit = $derived(cpuLimit.trim() === '' ? null : Number(cpuLimit));
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
		if (options && memLimitMB < options.min_memory_limit_mb) {
			problems.mem_limit_mb = `Use at least ${options.min_memory_limit_mb} MB.`;
		}
		if (parsedCPULimit !== null && (!Number.isFinite(parsedCPULimit) || parsedCPULimit <= 0)) {
			problems.cpu_limit = 'Use a positive CPU limit or leave it empty.';
		}
		return problems;
	});

	function problem(field: string): string | undefined {
		return localProblems[field] ?? apiError?.field(field);
	}

	const ready = $derived(
		canAdopt &&
			preview !== null &&
			name.trim() !== '' &&
			serverName.trim() !== '' &&
			worldName.trim() !== '' &&
			password.length >= minPassword &&
			memLimitMB >= (options?.min_memory_limit_mb ?? 4096) &&
			Object.keys(localProblems).length === 0 &&
			!busy
	);

	$effect(() => {
		if (!canAdopt) return;
		void Promise.all([adoption.preview(containerID), instances.options()])
			.then(([found, gameOptions]) => {
				preview = found;
				options = gameOptions;
				if (!name) name = found.instance_id;
			})
			.catch((err) => (failure = err));
	});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (!ready) return;
		busy = true;
		failure = null;
		const setModifiers = Object.fromEntries(
			Object.entries(modifiers).filter(([, value]) => value.trim() !== '')
		);
		const body: AdoptInstance = {
			name: name.trim(),
			server_name: serverName.trim(),
			world_name: worldName.trim(),
			password,
			public: isPublic,
			crossplay,
			preset,
			modifiers: setModifiers,
			extra_args: extraArgs,
			mem_limit_mb: memLimitMB,
			cpu_limit: parsedCPULimit
		};
		try {
			job = await adoption.adopt(containerID, body);
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	async function finished(result: Job) {
		if (result.status !== 'succeeded' || !result.instance_id) return;
		await Promise.all([instanceList.load(), session.refreshPermissions()]);
		await goto(resolve('/instances/[id]', { id: result.instance_id }));
	}
</script>

<main class="mx-auto grid max-w-2xl gap-4 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<h1 class="text-2xl font-semibold tracking-tight">Recover an existing server</h1>
	<Problem error={failure} />

	{#if !canAdopt}
		<p class="text-sm text-muted-foreground">You need administrator access to recover a server.</p>
	{:else if job}
		<Card.Root>
			<Card.Header>
				<Card.Title>Recovering {name}</Card.Title>
				<Card.Description>
					The existing container and its files stay in place. Valmin restores this server to the
					panel.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<JobProgress jobId={job.job_id} onfinish={finished} />
			</Card.Content>
		</Card.Root>
	{:else if preview}
		<Alert.Root>
			<ShieldCheck />
			<Alert.Title>Recoverable server found</Alert.Title>
			<Alert.Description>
				{preview.name} is {preview.running ? 'running' : 'stopped'} on UDP {preview.base_port}–{preview.base_port +
					1}, with game build {preview.game_build_id}{preview.modded
					? ' and mod loader files'
					: ''}.
			</Alert.Description>
		</Alert.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Confirm the server settings</Card.Title>
				<Card.Description>
					These values must describe the existing container exactly. Valmin checks these settings
					against the container before restoring the server to the panel. It never stops, recreates,
					copies, or changes this server during adoption.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<form onsubmit={submit} class="grid gap-4">
					<div class="grid gap-2">
						<Label for="name">Panel name</Label>
						<Input id="name" bind:value={name} autocomplete="off" />
						{#if problem('name')}<p class="text-sm text-destructive">{problem('name')}</p>{/if}
					</div>
					<div class="grid gap-2">
						<Label for="server-name">Server name</Label>
						<Input id="server-name" bind:value={serverName} autocomplete="off" />
						{#if problem('server_name')}
							<p class="text-sm text-destructive">{problem('server_name')}</p>
						{/if}
					</div>
					<div class="grid gap-2">
						<Label for="world-name">World name</Label>
						<Input id="world-name" bind:value={worldName} autocomplete="off" />
						{#if problem('world_name')}
							<p class="text-sm text-destructive">{problem('world_name')}</p>
						{/if}
					</div>
					<div class="grid gap-2">
						<Label for="password">Server password</Label>
						<Input
							id="password"
							type="password"
							bind:value={password}
							autocomplete="new-password"
						/>
						<p class="text-xs text-muted-foreground">
							The password is checked against the container but is never returned by the preview.
						</p>
						{#if problem('password')}
							<p class="text-sm text-destructive">{problem('password')}</p>
						{/if}
					</div>
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="public">List publicly</Label>
							<p class="text-xs text-muted-foreground">Show the server in the community browser.</p>
						</div>
						<Switch id="public" bind:checked={isPublic} />
					</div>
					<div class="flex items-center justify-between gap-4">
						<Label for="crossplay">Crossplay</Label>
						<Switch id="crossplay" bind:checked={crossplay} />
					</div>
					<div class="grid gap-2">
						<Label for="preset">World preset</Label>
						<Input id="preset" list="adoption-presets" bind:value={preset} autocomplete="off" />
						<datalist id="adoption-presets">
							{#each options?.presets ?? [] as value (value)}
								<option {value}></option>
							{/each}
						</datalist>
						<p class="text-xs text-muted-foreground">
							Leave empty for the server default. Known presets are suggested, but the list is not
							exhaustive.
						</p>
					</div>
					<div class="grid gap-2">
						<Label for="memory">Memory limit (MB)</Label>
						<Input
							id="memory"
							type="number"
							min={options?.min_memory_limit_mb ?? 4096}
							step="256"
							bind:value={memLimitMB}
						/>
						{#if problem('mem_limit_mb')}
							<p class="text-sm text-destructive">{problem('mem_limit_mb')}</p>
						{/if}
					</div>

					<button
						type="button"
						class="text-left text-sm font-medium"
						onclick={() => (advanced = !advanced)}
					>
						Advanced launch settings {advanced ? '−' : '+'}
					</button>
					{#if advanced}
						<div class="grid gap-2">
							<Label for="cpu">CPU limit</Label>
							<Input id="cpu" type="text" inputmode="decimal" bind:value={cpuLimit} />
							<p class="text-xs text-muted-foreground">
								Leave empty if the container has no CPU limit.
							</p>
							{#if problem('cpu_limit')}
								<p class="text-sm text-destructive">{problem('cpu_limit')}</p>
							{/if}
						</div>
						<div class="grid gap-2">
							<Label for="extra-args">Extra arguments</Label>
							<Input id="extra-args" bind:value={extraArgs} autocomplete="off" />
						</div>
						{#each options?.modifier_keys ?? [] as key (key)}
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

					<div class="flex justify-end gap-2">
						<Button variant="outline" href={resolve('/')}>Cancel</Button>
						<Button type="submit" disabled={!ready}>
							{busy ? 'Recovering…' : 'Recover server'}
						</Button>
					</div>
				</form>
			</Card.Content>
		</Card.Root>
	{:else}
		<p class="text-sm text-muted-foreground">Checking the container…</p>
	{/if}
</main>
