<script lang="ts">
	import { instances, type Instance, type WorldOnDisk } from '$lib/api/instances';
	import * as Alert from '$lib/components/ui/alert';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let { instance }: { instance: Instance } = $props();

	let worlds = $state<WorldOnDisk[]>([]);
	let loaded = $state(false);

	$effect(() => {
		void load(instance.id);
	});

	async function load(id: string) {
		try {
			worlds = await instances.worlds(id);
		} catch {
			// A listing that cannot be read says nothing, rather than claiming the savedir is
			// empty — which is the one answer here that would send an operator looking in the
			// wrong place.
			worlds = [];
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
		{#if worlds.length === 0}
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
					</li>
				{/each}
			</ul>
		{/if}

		{#if !configured}
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
