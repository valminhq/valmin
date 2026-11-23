<script lang="ts">
	import { resolve } from '$app/paths';
	import type { InstalledMod } from '#lib/api/mods.js';
	import { formatInstant } from '#lib/api/schedules.js';
	import { setups, type SavedSetup } from '#lib/api/setups.js';
	import { changeSummary } from '#lib/compare.js';

	// The newest automatic setup of a server, what changed in its mods since, and a link to roll
	// back to it. Shows nothing when there is none or nothing changed since.
	let { instanceId, installed }: { instanceId: string; installed: InstalledMod[] } = $props();

	let newest = $state<SavedSetup | null>(null);
	let request = 0;

	// Reloads each time the installed list is read again, which every mod change does.
	$effect(() => {
		void installed;
		void load(instanceId);
	});

	/** Reads the setups of instance id and keeps its newest automatic one. */
	async function load(id: string) {
		const mine = ++request;
		try {
			const list = await setups.list(id);
			if (mine === request) newest = list.items.find((setup) => setup.automatic) ?? null;
		} catch {
			if (mine === request) newest = null;
		}
	}

	const since = $derived(newest ? changeSummary(newest.mods, installed) : '');
</script>

{#if newest && since}
	<p class="flex flex-wrap items-baseline gap-x-2 text-sm" data-testid="last-mod-change">
		<span class="font-medium">{newest.name}</span>
		<span class="text-muted-foreground"
			>{formatInstant(newest.created_at)}{newest.created_by ? ` by ${newest.created_by}` : ''}</span
		>
		<span class="break-words">· since then: {since}</span>
		<a
			class="font-medium underline"
			href="{resolve('/instances/[id]/setups', { id: instanceId })}?setup={encodeURIComponent(
				newest.id
			)}">Undo</a
		>
	</p>
{/if}
