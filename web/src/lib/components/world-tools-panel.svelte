<script lang="ts">
	import { resolve } from '$app/paths';
	import {
		actions,
		instances,
		stateSentence,
		type CommandCapabilities,
		type Instance,
		type WorldToolRequest
	} from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	type Tool = WorldToolRequest['tool'];

	/** Suggestions for Upgrade World's upgrade operations; any other name can be typed. */
	const OPERATIONS = [
		'bearcave',
		'bogwitch',
		'combatruins',
		'hildir',
		'mountain_caves',
		'tarpits',
		'onions',
		'ashlands',
		'deepnorth',
		'mistlands'
	];

	const MODS: Record<Tool, { name: string; query: string }> = {
		upgrade_world: { name: 'JereKuusela-Upgrade_World', query: 'Upgrade_World' },
		fresh_world: { name: 'sighsorry-FreshWorld', query: 'FreshWorld' }
	};

	let caps = $state<CommandCapabilities | null>(null);
	let failure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let minDistance = $state<number | null>(null);
	let operation = $state('');
	let pending = $state<{ request: WorldToolRequest; title: string; effect: string } | null>(null);
	let confirmOpen = $state(false);

	const canRun = $derived(session.allowed(instance.id).includes(actions.worldTools));

	$effect(() => {
		if (canRun) void load(instance.id);
	});

	async function load(id: string) {
		try {
			caps = await instances.capabilities(id);
			failure = null;
		} catch (err) {
			failure = err;
		}
	}

	/** Why a tool's actions cannot run, with the mod to install when that is the reason. */
	function blocked(tool: Tool): { text: string; install?: { name: string; query: string } } | null {
		if (!caps) return { text: 'Checking which mods this server has.' };
		if (!caps.world_tools[tool]) {
			return { text: `Install ${MODS[tool].name} to use these actions.`, install: MODS[tool] };
		}
		if (caps.command_channel !== 'rcon') {
			return {
				text: 'Install Tristan-ValheimRcon: Valmin sends the command over RCON.',
				install: { name: 'Tristan-ValheimRcon', query: 'ValheimRcon' }
			};
		}
		if (jobRunning) return { text: 'A world tool is running. Wait for it to finish.' };
		if (instance.state !== 'stopped') {
			const now = instance.state === 'running' ? '' : `${stateSentence(instance.state)} `;
			return {
				text: `${now}Stop the server first: Valmin backs up the world, starts the server, and runs the command.`
			};
		}
		return null;
	}

	const upgradeBlocked = $derived(blocked('upgrade_world'));
	const freshBlocked = $derived(blocked('fresh_world'));
	const distanceValid = $derived(
		minDistance === null ||
			(Number.isInteger(minDistance) && minDistance >= 0 && minDistance <= 20000)
	);
	const operationValid = $derived(
		/^[a-z][a-z_]*$/.test(operation) && !operation.endsWith('_worldgen')
	);

	function ask(request: WorldToolRequest, title: string, effect: string) {
		pending = { request, title, effect };
		confirmOpen = true;
	}

	async function confirm() {
		if (!pending) return;
		const { request } = pending;
		confirmOpen = false;
		failure = null;
		try {
			const job = await instances.runWorldTool(instance.id, request);
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			failure = err;
		}
	}

	function finished() {
		jobRunning = false;
		onchange?.();
	}
</script>

{#snippet reason(why: ReturnType<typeof blocked>)}
	{#if why}
		<p class="text-sm text-muted-foreground">
			{why.text}
			{#if why.install}
				<a
					class="underline underline-offset-4"
					href={resolve(`/instances/[id]/mods?q=${encodeURIComponent(why.install.query)}`, {
						id: instance.id
					})}>Find it in mods</a
				>
			{/if}
		</p>
	{/if}
{/snippet}

{#if canRun}
	<Card.Root>
		<Card.Header>
			<Card.Title>World tools</Card.Title>
			<Card.Description>
				Mods that reset or upgrade parts of the world from inside the server. Each action backs up
				the world first, as a pre update backup, then starts the server and sends the mod's command.
				The server keeps running afterwards.
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-5">
			<section class="grid gap-3" aria-labelledby="upgrade-world">
				<h3 id="upgrade-world" class="text-sm font-semibold">Upgrade World</h3>
				{@render reason(upgradeBlocked)}
				<div class="grid gap-3">
					<div class="grid gap-2 rounded-lg border p-4">
						<p class="text-sm font-medium">Reset zones</p>
						<p class="text-sm text-muted-foreground">
							Removes everything in generated zones away from player bases, so they generate again
							when someone visits. Frees space and brings back resources.
						</p>
						<div class="grid gap-1">
							<Label for="uw-min">Only beyond this distance from the centre (m)</Label>
							<Input
								id="uw-min"
								type="number"
								min="0"
								max="20000"
								class="w-40"
								placeholder="Everywhere"
								bind:value={minDistance}
							/>
						</div>
						<Button
							size="sm"
							class="justify-self-start"
							disabled={upgradeBlocked !== null || !distanceValid}
							onclick={() =>
								ask(
									{
										tool: 'upgrade_world',
										action: 'zones_reset',
										...(minDistance ? { min_distance_m: minDistance } : {})
									},
									'Reset zones',
									'Generated zones away from player bases are emptied and generate again when visited.'
								)}>Reset zones</Button
						>
					</div>
					<div class="grid gap-2 rounded-lg border p-4">
						<p class="text-sm font-medium">Content upgrade</p>
						<p class="text-sm text-muted-foreground">
							Adds content from a game update to areas that were already explored.
						</p>
						<div class="grid gap-1">
							<Label for="uw-op">Operation</Label>
							<Input
								id="uw-op"
								list="uw-operations"
								class="w-56"
								placeholder="tarpits"
								bind:value={operation}
							/>
							<datalist id="uw-operations">
								{#each OPERATIONS as op (op)}<option value={op}></option>{/each}
							</datalist>
						</div>
						<Button
							size="sm"
							class="justify-self-start"
							disabled={upgradeBlocked !== null || !operationValid}
							onclick={() =>
								ask(
									{ tool: 'upgrade_world', action: 'upgrade', operation },
									`Run the ${operation} upgrade`,
									'Upgrade World adds or regenerates content in explored areas away from player bases.'
								)}>Upgrade</Button
						>
					</div>
					<div class="grid gap-2 rounded-lg border p-4">
						<p class="text-sm font-medium">World clean</p>
						<p class="text-sm text-muted-foreground">
							Removes objects and locations left by uninstalled mods, duplicates, and extra saved
							data.
						</p>
						<Button
							size="sm"
							class="justify-self-start"
							disabled={upgradeBlocked !== null}
							onclick={() =>
								ask(
									{ tool: 'upgrade_world', action: 'world_clean' },
									'Clean the world',
									'Missing objects, missing locations and duplicate objects are removed from the world.'
								)}>Clean world</Button
						>
					</div>
				</div>
			</section>

			<section class="grid gap-3" aria-labelledby="fresh-world">
				<h3 id="fresh-world" class="text-sm font-semibold">FreshWorld</h3>
				{@render reason(freshBlocked)}
				<div class="grid gap-2 rounded-lg border p-4">
					<p class="text-sm font-medium">Run now</p>
					<p class="text-sm text-muted-foreground">
						Restores zones, resources and locations the way the mod's own settings describe,
						protecting player bases.
					</p>
					<Button
						size="sm"
						class="justify-self-start"
						disabled={freshBlocked !== null}
						onclick={() =>
							ask(
								{ tool: 'fresh_world', action: 'run' },
								'Run FreshWorld',
								'FreshWorld restores the world using the settings in its own config file.'
							)}>Run FreshWorld</Button
					>
				</div>
			</section>

			<Problem error={failure} />

			{#if jobId}
				<div class="rounded-lg border p-4">
					<JobProgress {jobId} onfinish={finished} />
				</div>
			{/if}
		</Card.Content>
	</Card.Root>

	<DestructiveConfirm
		bind:open={confirmOpen}
		name={instance.world_name}
		title="{pending?.title ?? ''} on {instance.world_name}?"
		description="{pending?.effect ??
			''} Valmin backs up the world first, as a pre update backup, then starts the server and sends the command. The server keeps running afterwards."
		confirmLabel={pending?.title ?? 'Run'}
		onconfirm={confirm}
	/>
{/if}
