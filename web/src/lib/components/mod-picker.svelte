<script lang="ts">
	/**
	 * Choose packages from the synced catalogue. Used by the create wizard, where the server
	 * does not exist yet, so this component knows nothing about instances, jobs or install
	 * state — it searches, and it hands back a list.
	 *
	 * `↯` No Valheim in here (F2). "Mod", "package", "author" and "downloads" are the mod
	 * host's vocabulary and reach this file as catalogue fields; nothing about placement,
	 * loaders or the game is decided on this side (`02 §2.1`).
	 */
	import { mods, type ModSummary } from '$lib/api/mods';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Badge } from '$lib/components/ui/badge';
	import Plus from '@lucide/svelte/icons/plus';
	import X from '@lucide/svelte/icons/x';
	import Search from '@lucide/svelte/icons/search';

	/** The chosen packages, owned by the parent so the form submits them. */
	let { chosen = $bindable([]) }: { chosen: ModSummary[] } = $props();

	let query = $state('');
	let results = $state<ModSummary[]>([]);
	let nextCursor = $state<string | null>(null);
	let searching = $state(false);
	let failure = $state<string | null>(null);

	const compact = new Intl.NumberFormat(undefined, { notation: 'compact' });
	const chosenNames = $derived(new Set(chosen.map((m) => m.full_name)));

	// Search as the operator types, a quarter-second after they stop. The catalogue is local
	// to the daemon, so this is a database read and not a call to the mod host.
	$effect(() => {
		const q = query.trim();
		const timer = setTimeout(() => void search(q, null), 250);
		return () => clearTimeout(timer);
	});

	async function search(q: string, cursor: string | null) {
		searching = true;
		try {
			const found = await mods.search(q, cursor);
			results = cursor ? [...results, ...found.items] : found.items;
			nextCursor = found.next_cursor;
			failure = null;
		} catch {
			// The catalogue is a convenience on this screen and a server can be created
			// without it, so a failed search says so quietly instead of failing the wizard.
			failure = 'The mod catalogue could not be read. You can still create the server.';
		} finally {
			searching = false;
		}
	}

	function add(mod: ModSummary) {
		if (!chosenNames.has(mod.full_name)) chosen = [...chosen, mod];
	}

	function remove(fullName: string) {
		chosen = chosen.filter((m) => m.full_name !== fullName);
	}

	function hideBrokenIcon(event: Event) {
		(event.currentTarget as HTMLImageElement).hidden = true;
	}
</script>

{#snippet icon(mod: ModSummary, size: string)}
	<span
		class="grid shrink-0 place-items-center rounded-md border bg-muted text-sm font-medium text-muted-foreground {size}"
	>
		{mod.name.slice(0, 1).toUpperCase()}
		{#if mod.icon_url}
			<img
				src={mod.icon_url}
				alt=""
				loading="lazy"
				referrerpolicy="no-referrer"
				onerror={hideBrokenIcon}
				class="col-start-1 row-start-1 rounded-md {size}"
			/>
		{/if}
	</span>
{/snippet}

<div class="grid gap-3">
	{#if chosen.length > 0}
		<ul class="grid gap-2">
			{#each chosen as mod (mod.full_name)}
				<li class="flex items-center gap-3 rounded-lg border p-3">
					{@render icon(mod, 'size-8')}
					<div class="grid min-w-0 flex-1 gap-0.5">
						<div class="flex flex-wrap items-baseline gap-x-2">
							<span class="font-medium">{mod.name}</span>
							<span class="text-sm text-muted-foreground">by {mod.namespace}</span>
							<span class="text-sm text-muted-foreground tabular-nums">
								{mod.latest_version}
							</span>
						</div>
						{#if mod.is_deprecated}
							<span class="text-xs text-destructive"> The author marked this deprecated. </span>
						{/if}
					</div>
					<Button
						variant="ghost"
						size="sm"
						aria-label="Remove {mod.name}"
						onclick={() => remove(mod.full_name)}
					>
						<X />
					</Button>
				</li>
			{/each}
		</ul>
		<p class="text-xs text-muted-foreground">
			Anything these depend on is installed with them, and the loader is added automatically. They
			go on before the server first starts.
		</p>
	{/if}

	<div class="relative">
		<Search
			class="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
		/>
		<Input class="pl-9" placeholder="Search mods by name" bind:value={query} />
	</div>

	{#if failure}
		<p class="text-sm text-muted-foreground">{failure}</p>
	{:else if results.length === 0}
		<p class="text-sm text-muted-foreground">
			{#if searching}
				Searching…
			{:else if query}
				Nothing matches “{query}”. Try a shorter word.
			{:else}
				Nothing in the catalogue yet. It downloads on its own shortly after the panel starts.
			{/if}
		</p>
	{:else}
		<ul class="max-h-96 divide-y overflow-y-auto rounded-lg border">
			{#each results as mod (mod.full_name)}
				<li class="flex items-start gap-3 p-3">
					{@render icon(mod, 'size-10')}
					<div class="grid min-w-0 flex-1 gap-1">
						<div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
							<span class="font-medium">{mod.name}</span>
							<span class="text-sm text-muted-foreground">by {mod.namespace}</span>
							{#if mod.is_deprecated}
								<Badge variant="destructive">deprecated</Badge>
							{/if}
						</div>
						{#if mod.description}
							<p class="max-w-prose text-sm text-muted-foreground">{mod.description}</p>
						{/if}
						<p class="text-xs text-muted-foreground tabular-nums">
							{mod.latest_version} · {compact.format(mod.downloads)} downloads
						</p>
					</div>
					<Button
						class="shrink-0"
						variant="outline"
						size="sm"
						disabled={chosenNames.has(mod.full_name)}
						onclick={() => add(mod)}
					>
						{#if chosenNames.has(mod.full_name)}
							Added
						{:else}
							<Plus />
							Add
						{/if}
					</Button>
				</li>
			{/each}
		</ul>
		{#if nextCursor}
			<Button
				variant="outline"
				size="sm"
				disabled={searching}
				onclick={() => void search(query.trim(), nextCursor)}
			>
				Show more
			</Button>
		{/if}
	{/if}
</div>
