<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import {
		mods,
		type InstalledMod,
		type ModSummary,
		type PluginLoad,
		type ResolvedNode
	} from '$lib/api/mods';
	import { session } from '$lib/state/session.svelte';
	import { socket, socketStatus } from '$lib/socket/index.svelte';
	import { topics, type ServerMessage } from '$lib/socket/messages';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import * as Alert from '$lib/components/ui/alert';
	import * as Dialog from '$lib/components/ui/dialog';
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import CircleCheck from '@lucide/svelte/icons/circle-check';
	import Download from '@lucide/svelte/icons/download';
	import Search from '@lucide/svelte/icons/search';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let installed = $state<InstalledMod[]>([]);
	let boot = $state<PluginLoad | null>(null);
	let loading = $state(true);
	let failure = $state<unknown>(null);

	/**
	 * What the catalogue says about each installed mod, read one package at a time.
	 *
	 * `GET /instances/{id}/mods` carries no catalogue row (Q39), so "a newer version
	 * exists" and "the author deprecated this" — the two things an operator most wants to be
	 * told without going looking for them — are read here instead. A package the index has
	 * never heard of is simply absent from the map: before the first sync, and for a package
	 * that has been pulled, the panel knows nothing and says nothing rather than reporting
	 * the installed version as current.
	 */
	let catalogue = $state(new Map<string, ModSummary>());

	let query = $state('');
	let results = $state<ModSummary[]>([]);
	let nextCursor = $state<string | null>(null);
	let syncedAt = $state<string | null>(null);
	let searching = $state(false);

	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let resolvingName = $state<string | null>(null);
	let confirming = $state<{ target: ModSummary; nodes: ResolvedNode[] } | null>(null);
	let confirmOpen = $state(false);
	let removing = $state<InstalledMod | null>(null);
	let removeOpen = $state(false);
	let removeOrphans = $state(false);

	const allowed = $derived(session.allowed(id));
	const canManage = $derived(allowed.includes(actions.modsManage));

	/**
	 * Why every mod action is unavailable right now, or null when they are available.
	 *
	 * B11 / C19: mods are applied to a stopped server, and no job here will stop one for
	 * the operator. The server refuses independently — this exists so the refusal is legible
	 * before the click rather than after it.
	 */
	const blocked = $derived.by(() => {
		if (jobRunning) return 'A mod change is running. Wait for it to finish.';
		if (!instance) return 'Loading this server.';
		if (instance.state === 'running') {
			return 'This server is running. Stop it to install or remove mods.';
		}
		if (instance.state !== 'stopped') {
			return `This server is ${instance.state.replaceAll('_', ' ')}. Mods change only on a stopped server.`;
		}
		return null;
	});
	const canAct = $derived(canManage && blocked === null);

	/** The one thing a failed request knows that its generic message does not say (D10):
	 * which packages stand in the way, or which one is missing from the index. */
	const detail = $derived.by(() => {
		if (!(failure instanceof ApiError)) return null;
		const by = failure.details.required_by;
		if (Array.isArray(by) && by.length > 0) {
			return `Still needed by ${by.join(', ')}. Remove those first.`;
		}
		const missing = failure.details.missing;
		return typeof missing === 'string' && missing ? `Not in the index: ${missing}.` : null;
	});

	// Chosen mods above, the packages they dragged in behind a disclosure: an operator picked
	// three things and got fourteen, and the three are what they came to manage.
	const chosen = $derived(installed.filter((m) => m.installed_as !== 'dependency'));
	const dependencies = $derived(installed.filter((m) => m.installed_as === 'dependency'));
	const notLoading = $derived(installed.filter((m) => m.load_status === 'not_seen'));
	const installedNames = $derived(new Set(installed.map((m) => m.full_name)));
	const installedVersions = $derived(new Map(installed.map((m) => [m.full_name, m.version])));

	async function refresh() {
		try {
			instance = await instances.get(id);
			const listed = await mods.installed(id);
			installed = listed.mods;
			boot = listed.plugin_load;
			failure = null;
			void readCatalogue(listed.mods);
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	// One read per installed package, in parallel, and a failure on any of them is silence:
	// these decorate rows that are already correct without them, so they must never be able
	// to fail the page.
	async function readCatalogue(rows: InstalledMod[]) {
		const found = await Promise.all(
			rows.map(async (mod) => {
				// No namespace means the daemon's catalogue has no row for it, so there is
				// nothing to ask for and nothing to decorate the row with.
				if (!mod.namespace) return null;
				try {
					return [mod.full_name, await mods.detail(mod.namespace, mod.name)] as const;
				} catch {
					return null;
				}
			})
		);
		catalogue = new Map(found.filter((entry) => entry !== null));
	}

	// Subscribe, then fetch (G3, `14 §7.2`). The state topic is what tells this page the
	// server was started from another tab, which is the difference between a disabled
	// install button and a 409 the operator has to read.
	$effect(() => {
		const off = socket.subscribe(topics.state(id), (m: ServerMessage) => {
			if (m.type !== 'state' || !instance) return;
			instance = { ...instance, state: m.state, restart_required: m.restart_required };
		});
		void refresh();
		return off;
	});

	let lastStatus = $state(socketStatus.value);
	$effect(() => {
		const status = socketStatus.value;
		if (status === 'open' && lastStatus !== 'open') void refresh();
		lastStatus = status;
	});

	// The index is the panel's own copy (`04 §3`), so an empty query is a browse of what is
	// there rather than a wasted round trip to Thunderstore.
	$effect(() => {
		const q = query;
		const timer = setTimeout(() => void search(q, null), 250);
		return () => clearTimeout(timer);
	});

	async function search(q: string, cursor: string | null) {
		searching = true;
		try {
			const found = await mods.search(q, cursor);
			results = cursor ? [...results, ...found.items] : found.items;
			nextCursor = found.next_cursor;
			syncedAt = found.synced_at;
		} catch (err) {
			failure = err;
		} finally {
			searching = false;
		}
	}

	async function askToInstall(target: ModSummary) {
		failure = null;
		resolvingName = target.full_name;
		try {
			const closure = await mods.resolve(id, target.full_name, target.latest_version);
			confirming = { target, nodes: closure.nodes };
			confirmOpen = true;
		} catch (err) {
			failure = err;
		} finally {
			resolvingName = null;
		}
	}

	function askToRemove(mod: InstalledMod) {
		failure = null;
		removeOrphans = false;
		removing = mod;
		removeOpen = true;
	}

	async function start(action: () => Promise<{ job_id: string }>) {
		failure = null;
		try {
			const job = await action();
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			failure = err;
		}
	}

	function installConfirmed() {
		const pending = confirming;
		confirmOpen = false;
		if (!pending) return;
		void start(() => mods.install(id, pending.target.full_name, pending.target.latest_version));
	}

	function removeConfirmed() {
		const pending = removing;
		const orphans = removeOrphans;
		removeOpen = false;
		if (!pending) return;
		void start(() => mods.uninstall(id, pending.full_name, orphans));
	}

	/** The action a browse row offers: nothing new to do, a version change, or an install.
	 * The comparison is string equality and deliberately nothing cleverer — deciding
	 * which of two versions is newer is the resolver's job, on the server (F2). */
	function offer(mod: ModSummary): 'install' | 'update' | 'installed' {
		const version = installedVersions.get(mod.full_name);
		if (version === undefined) return 'install';
		return version === mod.latest_version ? 'installed' : 'update';
	}

	const dateFormat = new Intl.DateTimeFormat(undefined, {
		dateStyle: 'medium',
		timeStyle: 'short'
	});
	const compact = new Intl.NumberFormat(undefined, { notation: 'compact' });

	function when(timestamp: string | null): string {
		if (!timestamp) return '';
		const date = new Date(timestamp);
		return Number.isNaN(date.getTime()) ? '' : dateFormat.format(date);
	}

	function hideBrokenIcon(event: Event) {
		(event.currentTarget as HTMLImageElement).hidden = true;
	}
</script>

<div class="mx-auto grid max-w-4xl gap-6 p-6">
	<header class="grid gap-3">
		<Button
			variant="ghost"
			size="sm"
			class="justify-self-start"
			href={resolve('/instances/[id]', { id })}
		>
			<ArrowLeft />
			{instance?.name ?? 'Server'}
		</Button>
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h1 class="text-lg font-semibold">Mods</h1>
				<p class="text-sm text-muted-foreground">
					What this server loads, and the catalogue to add from.
				</p>
			</div>
			{#if instance}
				<StateBadge state={instance.state} restartRequired={instance.restart_required} />
			{/if}
		</div>
	</header>

	<Problem error={failure} />
	{#if detail}
		<p class="-mt-4 text-sm text-muted-foreground">{detail}</p>
	{/if}

	<!--
		The page opens on the answer to the question that brings an operator here — did
		the mods actually load — rather than on a catalogue. `03 §5.2`'s failure mode is a
		server that boots perfectly and loads nothing, so a screen that led with a browse grid
		would make the one thing worth knowing the one thing you have to go looking for.
	-->
	{#if installed.length === 0}
		<!-- Nothing installed: the verdict has nothing to be about, and the empty state below
		     is the whole message. -->
	{:else if notLoading.length > 0 || boot?.discrepancy}
		<Alert.Root variant="destructive">
			<TriangleAlert />
			<Alert.Title>
				{#if notLoading.length > 0}
					{notLoading.length} of {installed.length} mods did not load
				{:else}
					Fewer mods loaded than this server announced
				{/if}
			</Alert.Title>
			<Alert.Description class="grid gap-1">
				{#if notLoading.length > 0}
					<span>{notLoading.map((m) => m.full_name).join(', ')}</span>
				{/if}
				{#if boot?.discrepancy}
					<span>{boot.discrepancy}.</span>
				{/if}
				<span class="text-xs">Checked {when(boot?.observed_at ?? null)}</span>
			</Alert.Description>
		</Alert.Root>
	{:else if boot}
		<div class="flex flex-wrap items-center gap-3 rounded-lg border bg-card p-4">
			<CircleCheck class="size-5 text-muted-foreground" />
			<div class="grid gap-0.5">
				<!-- No count in the headline. `boot.loaded` counts the plugin lines the loader
				     printed, which is not the number of installed packages — one package can
				     place several, and a framework or config-only package places none. The
				     number is reported as what it is, beside the claim rather than inside it. -->
				<span class="font-medium">Everything loaded</span>
				<span class="text-xs text-muted-foreground">
					{boot.loaded}
					{boot.loaded === 1 ? 'plugin' : 'plugins'} loaded when this server last started, {when(
						boot.observed_at
					)}
				</span>
			</div>
		</div>
	{:else}
		<div class="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">
			No load report yet. Start this server to find out which of its mods load.
		</div>
	{/if}

	{#if instance?.restart_required}
		<!-- ADR-012: the mods on disk and the mods in memory have diverged, and only a restart
		     closes that. Said here as well as on the server page because this is the screen
		     that just caused it. -->
		<Alert.Root>
			<TriangleAlert />
			<Alert.Title>Restart required</Alert.Title>
			<Alert.Description>
				Mods changed since this server started. The running server is still using the old set.
			</Alert.Description>
		</Alert.Root>
	{/if}

	{#if jobId}
		<!-- F4: no optimistic UI. This is the job the daemon reports, including the flat
		     stretch while a package downloads. -->
		<div class="rounded-lg border p-4">
			<JobProgress
				{jobId}
				onfinish={() => {
					jobRunning = false;
					void refresh();
				}}
			/>
		</div>
	{/if}

	<section class="grid gap-3">
		<div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
			<h2 class="font-medium">Installed</h2>
			{#if canManage && blocked}
				<p class="text-sm text-muted-foreground" data-testid="mod-actions-blocked">{blocked}</p>
			{/if}
		</div>

		{#if loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if installed.length === 0}
			<div class="grid gap-1 rounded-lg border border-dashed p-6 text-center">
				<p class="font-medium">No mods yet</p>
				<p class="text-sm text-muted-foreground">
					Search the catalogue below and install the first one.
				</p>
			</div>
		{:else}
			<ul class="divide-y rounded-lg border">
				{#each chosen as mod (mod.full_name)}
					<li class="flex flex-wrap items-center gap-x-3 gap-y-1 p-4">
						{@render installedRow(mod)}
					</li>
				{/each}

				{#if dependencies.length > 0}
					<!-- Native disclosure: keyboard-operable, open by default when something in it
					     needs attention. -->
					<li>
						<details class="group" open={dependencies.some((m) => m.load_status === 'not_seen')}>
							<summary
								class="flex cursor-pointer list-none items-center gap-2 p-4 text-sm text-muted-foreground hover:text-foreground"
							>
								<ChevronRight
									class="size-4 transition-transform group-open:rotate-90 motion-reduce:transition-none"
								/>
								{dependencies.length}
								{dependencies.length === 1 ? 'dependency' : 'dependencies'} came with them
							</summary>
							<ul class="divide-y border-t">
								{#each dependencies as mod (mod.full_name)}
									<li class="flex flex-wrap items-center gap-x-3 gap-y-1 bg-muted/30 p-4">
										{@render installedRow(mod)}
									</li>
								{/each}
							</ul>
						</details>
					</li>
				{/if}
			</ul>
		{/if}
	</section>

	<section class="grid gap-3">
		<div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
			<h2 class="font-medium">{canManage ? 'Add a mod' : 'Catalogue'}</h2>
			<span class="text-sm text-muted-foreground">
				{syncedAt ? `Catalogue updated ${when(syncedAt)}` : 'The catalogue has not downloaded yet.'}
			</span>
		</div>

		<div class="relative">
			<Search
				class="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
			/>
			<Input
				bind:value={query}
				class="pl-9"
				placeholder="Search by name or description"
				aria-label="Search mods"
			/>
		</div>

		{#if results.length === 0}
			<p class="text-sm text-muted-foreground">
				{#if searching}
					Searching…
				{:else if query}
					Nothing matches “{query}”. Try a shorter word.
				{:else}
					Nothing in the catalogue yet. It downloads on its own once an hour.
				{/if}
			</p>
		{:else}
			<ul class="divide-y rounded-lg border">
				{#each results as mod (mod.full_name)}
					{@const state = offer(mod)}
					<li class="flex items-start gap-3 p-4">
						<!-- The package's own icon, from the catalogue row the sync derived. It is
						     fetched from the mod host, so a broken or blocked one leaves the initial
						     behind it rather than a broken-image glyph. -->
						<span
							class="grid size-10 shrink-0 place-items-center rounded-md border bg-muted text-sm font-medium text-muted-foreground"
						>
							{mod.name.slice(0, 1).toUpperCase()}
							{#if mod.icon_url}
								<img
									src={mod.icon_url}
									alt=""
									loading="lazy"
									referrerpolicy="no-referrer"
									onerror={hideBrokenIcon}
									class="col-start-1 row-start-1 size-10 rounded-md"
								/>
							{/if}
						</span>

						<div class="grid min-w-0 flex-1 gap-1">
							<div class="flex flex-wrap items-center gap-x-2 gap-y-1">
								<span class="font-medium">{mod.name}</span>
								<span class="text-sm text-muted-foreground">by {mod.namespace}</span>
								{#if mod.is_deprecated}
									<Badge variant="destructive">deprecated</Badge>
								{/if}
								{#if installedNames.has(mod.full_name)}
									<Badge variant="outline">installed</Badge>
								{/if}
							</div>
							{#if mod.description}
								<p class="max-w-prose text-sm text-muted-foreground">{mod.description}</p>
							{/if}
							<p class="text-xs text-muted-foreground tabular-nums">
								{mod.latest_version} · {compact.format(mod.downloads)} downloads
							</p>
						</div>

						{#if canManage}
							<Button
								class="shrink-0"
								variant={state === 'update' ? 'default' : 'outline'}
								size="sm"
								disabled={!canAct || state === 'installed' || resolvingName !== null}
								onclick={() => askToInstall(mod)}
							>
								{#if resolvingName === mod.full_name}
									Checking…
								{:else if state === 'installed'}
									Installed
								{:else if state === 'update'}
									Update
								{:else}
									<Download />
									Install
								{/if}
							</Button>
						{/if}
					</li>
				{/each}
			</ul>
			{#if nextCursor}
				<Button
					variant="outline"
					size="sm"
					class="justify-self-start"
					disabled={searching}
					onclick={() => void search(query, nextCursor)}
				>
					Show more
				</Button>
			{/if}
		{/if}
	</section>
</div>

{#snippet installedRow(mod: InstalledMod)}
	{@const listing = catalogue.get(mod.full_name)}
	{@const newer = listing && listing.latest_version !== mod.version ? listing : null}
	<div class="grid min-w-0 flex-1 gap-1">
		<div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
			<span class="font-medium">{mod.name || mod.full_name}</span>
			{#if mod.namespace}
				<span class="text-sm text-muted-foreground">by {mod.namespace}</span>
			{/if}
			<span class="text-sm text-muted-foreground tabular-nums">{mod.version}</span>
			{#if newer}
				<Badge variant="outline">{newer.latest_version} available</Badge>
			{/if}
			{#if listing?.is_deprecated}
				<Badge variant="destructive">deprecated</Badge>
			{/if}
			{#if mod.load_status === 'not_seen'}
				<Badge variant="destructive">not loading</Badge>
			{:else if mod.load_status === 'loaded'}
				<span class="inline-flex items-center gap-1 text-xs text-muted-foreground">
					<CircleCheck class="size-3" />
					loaded
				</span>
			{/if}
			{#if mod.side !== 'unknown'}
				<Badge variant="secondary">{mod.side.replaceAll('_', ' ')}</Badge>
			{/if}
		</div>
		<p class="text-xs text-muted-foreground">
			{mod.file_count}
			{mod.file_count === 1 ? 'file' : 'files'} · added {when(mod.installed_at)}
		</p>
	</div>
	{#if canManage}
		{#if newer}
			<Button
				variant="outline"
				size="sm"
				disabled={!canAct || resolvingName !== null}
				onclick={() => askToInstall(newer)}
			>
				{#if resolvingName === newer.full_name}
					Checking…
				{:else}
					Update
				{/if}
			</Button>
		{/if}
		<Button
			variant="ghost"
			size="sm"
			disabled={!canAct}
			onclick={() => askToRemove(mod)}
			aria-label="Remove {mod.full_name}"
		>
			<Trash2 />
			Remove
		</Button>
	{/if}
{/snippet}

<!--
	The closure, before anything downloads. `04 §3` puts resolve ahead of install for
	exactly this: installing one mod can place four packages, and the operator agrees to the
	list rather than discovering it afterwards in a file manifest.
-->
<Dialog.Root bind:open={confirmOpen}>
	<Dialog.Content>
		{#if confirming}
			{@const pending = confirming}
			<Dialog.Header>
				<Dialog.Title>Install {pending.target.name}?</Dialog.Title>
				<Dialog.Description>
					{pending.nodes.length === 1
						? 'One package will be installed.'
						: `${pending.nodes.length} packages will be installed, including everything it needs.`}
				</Dialog.Description>
			</Dialog.Header>
			<ul class="grid max-h-64 gap-2 overflow-y-auto text-sm">
				{#each pending.nodes as node (node.full_name)}
					<li class="flex flex-wrap items-center gap-2">
						<span class="font-medium">{node.full_name}</span>
						<span class="text-muted-foreground tabular-nums">{node.version}</span>
						{#if node.no_op}
							<Badge variant="secondary">already installed</Badge>
						{:else if node.transitive}
							<Badge variant="outline">dependency</Badge>
						{/if}
					</li>
				{/each}
			</ul>
			{#if pending.target.is_deprecated}
				<p class="text-sm text-destructive">
					The author has marked this mod deprecated. It may not work on the current game build.
				</p>
			{/if}
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (confirmOpen = false)}>Cancel</Button>
				<Button onclick={installConfirmed}>Install</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>

<!--
	F5: the confirmation names the mod being removed and says what removal is bounded by.
	It does not make the operator type the name back the way deleting a server does — that
	dialog guards a world, this one guards files the panel can fetch again.
-->
<Dialog.Root bind:open={removeOpen}>
	<Dialog.Content>
		{#if removing}
			{@const pending = removing}
			<Dialog.Header>
				<Dialog.Title>Remove {pending.full_name}?</Dialog.Title>
				<Dialog.Description>
					The {pending.file_count}
					{pending.file_count === 1 ? 'file' : 'files'} it placed are deleted. Settings you have edited
					stay, and the world is not touched.
				</Dialog.Description>
			</Dialog.Header>
			<div class="flex items-start gap-2 rounded-md border p-3">
				<input
					id="remove-orphans"
					type="checkbox"
					class="mt-0.5 size-4 accent-primary"
					bind:checked={removeOrphans}
				/>
				<Label for="remove-orphans" class="grid gap-1 text-sm font-normal">
					Also remove what it brought with it
					<span class="text-xs text-muted-foreground">
						Only the dependencies no other mod still needs.
					</span>
				</Label>
			</div>
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (removeOpen = false)}>Cancel</Button>
				<Button variant="destructive" onclick={removeConfirmed}>Remove</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
