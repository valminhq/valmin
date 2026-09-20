<script lang="ts">
	import { actions, instances, type Instance, type WorldOnDisk } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import * as Alert from '$lib/components/ui/alert';
	import { Button } from '$lib/components/ui/button';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import Problem from '$lib/components/problem.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import History from '@lucide/svelte/icons/history';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let worlds = $state<WorldOnDisk[]>([]);
	let loaded = $state(false);
	let picked = $state<WorldOnDisk | null>(null);
	let confirming = $state(false);
	let doomed = $state<WorldOnDisk | null>(null);
	let confirmingDelete = $state(false);
	let failure = $state<unknown>(null);
	let loadFailure = $state<unknown>(null);

	const allowed = $derived(session.allowed(instance.id));
	/** Replacing the live world is the same capability as importing one, because it is the
	 * same job. It needs the server stopped for the same reason (C19). */
	const canRestore = $derived(
		allowed.includes(actions.worldImport) && instance.state === 'stopped'
	);
	/** Deleting a world is the same capability and the same precondition as replacing one. */
	const canDelete = $derived(canRestore);

	async function remove(world: WorldOnDisk) {
		failure = null;
		try {
			await instances.deleteWorldOnDisk(instance.id, world.name);
			await load(instance.id);
			onchange?.();
		} catch (err) {
			failure = err;
		}
	}

	async function restore(world: WorldOnDisk) {
		failure = null;
		try {
			await instances.restoreWorldOnDisk(instance.id, world.name);
			await load(instance.id);
			onchange?.();
		} catch (err) {
			failure = err;
		}
	}

	$effect(() => {
		void load(instance.id);
	});

	/** A listing that cannot be read is kept as a failure, never as an empty savedir: both the
	 * empty sentence and the missing-world alert below are claims about the disk that a
	 * transport error does not license. */
	async function load(id: string) {
		loadFailure = null;
		try {
			worlds = await instances.worlds(id);
		} catch (err) {
			worlds = [];
			loadFailure = err;
		} finally {
			loaded = true;
		}
	}

	const configured = $derived(worlds.find((w) => w.loaded));

	function size(bytes: number): string {
		const units = ['B', 'KB', 'MB', 'GB'];
		let v = bytes;
		let u = 0;
		while (v >= 1024 && u < units.length - 1) {
			v /= 1024;
			u++;
		}
		return `${v.toFixed(u === 0 ? 0 : 1)} ${units[u]}`;
	}
</script>

<!--
	A backup verifies that the archive carries this server's world, and refuses otherwise
	(`02 §4.4` step 5). From the operator's side that refusal names two files that are not
	there and nothing about what is — so this says what the savedir actually holds, beside the
	catalogue where the refusal is read.
-->
{#if loaded}
	<div class="grid gap-2">
		<h3 class="text-sm font-medium">Worlds on disk</h3>
		{#if loadFailure}
			<Problem error={loadFailure} />
			<Button
				variant="outline"
				size="sm"
				class="justify-self-start"
				onclick={() => load(instance.id)}
			>
				Retry reading the save directory
			</Button>
		{:else if worlds.length === 0}
			<p class="text-sm text-muted-foreground">
				Nothing in this server’s save directory yet. A world appears here once the server has
				started and saved one, or once you import one.
			</p>
		{:else}
			<ul class="grid gap-1 text-sm">
				{#each worlds as world (world.dir + '/' + world.name)}
					<li class="flex flex-wrap items-baseline gap-x-2">
						<span class="font-mono text-xs">{world.name}</span>
						{#if world.loaded}
							<span class="text-xs text-muted-foreground">· loaded by this server</span>
						{/if}
						<span class="text-xs text-muted-foreground">
							· {size(world.bytes)}
							{#if !world.complete}· incomplete: its {world.db_bytes === null
									? 'world data'
									: 'header'} is missing{/if}
						</span>
						<span class="ml-auto flex items-center gap-1">
							{#if canRestore && !world.loaded && world.complete}
								<Button
									variant="ghost"
									size="sm"
									class="h-6 px-2 text-xs"
									onclick={() => ((picked = world), (confirming = true))}
								>
									<History />
									Load this one instead
								</Button>
							{/if}
							{#if canDelete}
								<Button
									variant="ghost"
									size="sm"
									class="h-6 px-2 text-xs text-destructive hover:text-destructive"
									onclick={() => ((doomed = world), (confirmingDelete = true))}
								>
									<Trash2 />
									Delete
								</Button>
							{/if}
						</span>
					</li>
				{/each}
			</ul>
		{/if}

		<Problem error={failure} />

		<!--
			The game keeps rolling saves beside the live world, and until now going back to one
			meant fetching it off the host and uploading it again. It is the same job an upload
			runs, so the world being replaced is archived first.
		-->
		{#if picked}
			{@const world = picked}
			<DestructiveConfirm
				bind:open={confirming}
				name={instance.name}
				title="Replace this server's world?"
				description={`${instance.name} will load ${world.name} in place of its current ${instance.world_name}. The world it is replacing is backed up first, so this can be undone from the backups list. Type the server's name to confirm.`}
				confirmLabel="Replace the world"
				onconfirm={() => {
					confirming = false;
					void restore(world);
				}}
			/>
		{/if}

		<!--
			Deleting the world a server loads is how it is reset: the next start generates a fresh
			one. The savedir is archived first, so it is undoable from the backups list.
		-->
		{#if doomed}
			{@const world = doomed}
			<DestructiveConfirm
				bind:open={confirmingDelete}
				name={instance.name}
				title={world.loaded ? "Reset this server's world?" : 'Delete this world?'}
				description={`${world.name} is removed from ${instance.name}'s save directory. Everything in the directory is backed up first, so this can be undone from the backups list.${world.loaded ? ` ${instance.name} loads this world, so it will generate a new one the next time it starts.` : ''} Type the server's name to confirm.`}
				confirmLabel={world.loaded ? 'Reset the world' : 'Delete the world'}
				onconfirm={() => {
					confirmingDelete = false;
					void remove(world);
				}}
			/>
		{/if}

		{#if !loadFailure && !configured}
			<Alert.Root variant="destructive">
				<TriangleAlert />
				<Alert.Title>This server’s world is not here</Alert.Title>
				<Alert.Description>
					It is set to load <span class="font-mono">{instance.world_name}</span>, and no world by
					that name is in its save directory. Backups will refuse, because there is nothing to
					archive. Start the server so it creates one, or import a world under that name.
				</Alert.Description>
			</Alert.Root>
		{/if}
	</div>
{/if}
