<script lang="ts">
	import { Tabs } from 'bits-ui';
	import { modOffer, catalogueStatus, installedUpdateTarget } from '$lib/mod-catalogue';
	import { page } from '$app/state';
	import { ApiError } from '$lib/api/errors';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import {
		modSides,
		modSources,
		mods,
		sourceBadge,
		sourceLabel,
		sourceText,
		type InstalledMod,
		type ModInstallTarget,
		type ModSide,
		type ModSource,
		type RegistryStatus,
		type ExportPreview,
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
	import * as Select from '$lib/components/ui/select';
	import * as Alert from '$lib/components/ui/alert';
	import * as Dialog from '$lib/components/ui/dialog';
	import Problem from '$lib/components/problem.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import CircleCheck from '@lucide/svelte/icons/circle-check';
	import Download from '@lucide/svelte/icons/download';
	import ArrowUpCircle from '@lucide/svelte/icons/arrow-up-circle';
	import Search from '@lucide/svelte/icons/search';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');

	let activeTab = $state('installed');
	let instance = $state<Instance | null>(null);
	let installed = $state<InstalledMod[]>([]);
	let boot = $state<PluginLoad | null>(null);
	let loading = $state(true);
	let failure = $state<unknown>(null);

	let query = $state('');
	/** Which registry the browse list is narrowed to, or null for every one of them. */
	let registry = $state<ModSource | null>(null);
	let registries = $state<RegistryStatus[]>([]);
	const catalogueMessage = $derived(catalogueStatus(registries, registry));
	let results = $state<ModSummary[]>([]);
	let nextCursor = $state<string | null>(null);
	let syncedAt = $state<string | null>(null);
	let searching = $state(false);
	let searchRequest = 0;

	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let resolvingName = $state<string | null>(null);
	let confirming = $state<{ target: ModInstallTarget; nodes: ResolvedNode[] } | null>(null);
	let confirmOpen = $state(false);
	let removing = $state<InstalledMod | null>(null);
	let removeOpen = $state(false);
	let removeOrphans = $state(false);
	let taggingName = $state<string | null>(null);
	let clientExport = $state<ExportPreview | null>(null);
	let exportFailure = $state<unknown>(null);
	let exportLoading = $state(false);

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
		if (taggingName !== null) return 'A mod label is being saved.';
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
	/** A label is recorded and read by nothing on disk (Q37), so it is not what `blocked`
	 * describes: the operator learns which mods their players need while the server is up,
	 * and tagging waits only on a mod change that is already in flight. */
	const canTag = $derived(canManage && instance !== null && !jobRunning && taggingName === null);

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
	const installedByName = $derived(new Map(installed.map((m) => [m.full_name, m])));

	async function refresh() {
		try {
			instance = await instances.get(id);
			const listed = await mods.installed(id);
			installed = listed.mods;
			boot = listed.plugin_load;
			failure = null;
			void readClientExport();
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function readClientExport() {
		exportLoading = true;
		exportFailure = null;
		try {
			clientExport = await mods.exportPreview(id);
		} catch (err) {
			exportFailure = err;
			clientExport = null;
		} finally {
			exportLoading = false;
		}
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
		const source = registry;
		const timer = setTimeout(() => void search(q, source, null), 250);
		return () => clearTimeout(timer);
	});

	async function search(q: string, source: ModSource | null, cursor: string | null) {
		const request = ++searchRequest;
		searching = true;
		try {
			const found = await mods.search(q, source, cursor);
			if (request !== searchRequest) return;
			results = cursor ? [...results, ...found.items] : found.items;
			nextCursor = found.next_cursor;
			syncedAt = found.synced_at;
			registries = found.registries ?? [];
			failure = null;
		} catch (err) {
			if (request !== searchRequest) return;
			failure = err;
		} finally {
			if (request === searchRequest) searching = false;
		}
	}

	async function askToInstall(target: ModInstallTarget) {
		failure = null;
		resolvingName = target.full_name;
		try {
			const closure = await mods.resolve(
				id,
				target.full_name,
				target.latest_version,
				target.source
			);
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
		if (!pending || !pending.nodes.some((node) => !node.no_op)) return;
		void start(() =>
			mods.install(
				id,
				pending.target.full_name,
				pending.target.latest_version,
				pending.target.source
			)
		);
	}

	function removeConfirmed() {
		const pending = removing;
		const orphans = removeOrphans;
		removeOpen = false;
		if (!pending) return;
		void start(() => mods.uninstall(id, pending.full_name, orphans));
	}

	async function setSide(mod: InstalledMod, side: ModSide) {
		if (side === mod.side) return;
		failure = null;
		taggingName = mod.full_name;
		try {
			await mods.setSide(id, mod.full_name, side);
			await refresh();
		} catch (err) {
			failure = err;
		} finally {
			taggingName = null;
		}
	}

	function sideLabel(side: ModSide): string {
		return modSides.find((option) => option.value === side)?.label ?? side;
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
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h2 class="text-2xl font-semibold tracking-tight">Mods</h2>
				<p class="text-sm text-muted-foreground">
					What this server loads, and the catalogue to add from.
				</p>
			</div>
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

	{#if canManage && blocked}
		<p class="text-sm text-muted-foreground" data-testid="mod-actions-blocked">{blocked}</p>
	{/if}
	<Tabs.Root bind:value={activeTab} class="grid gap-5">
		<Tabs.List aria-label="Mod tasks" class="flex w-fit flex-wrap gap-1 rounded-lg bg-muted p-1">
			<Tabs.Trigger
				value="installed"
				class="rounded-md px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-sm"
				>Installed {#if !loading}<span class="ml-1 tabular-nums">{installed.length}</span
					>{/if}</Tabs.Trigger
			>
			<Tabs.Trigger
				value="browse"
				class="rounded-md px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-sm"
				>Browse mods</Tabs.Trigger
			>
			<Tabs.Trigger
				value="players"
				class="rounded-md px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-sm"
				>Player modpack</Tabs.Trigger
			>
		</Tabs.List>
		<Tabs.Content value="installed" class="grid gap-3 data-[state=inactive]:hidden">
			<section class="grid gap-3">
				<div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
					<h2 class="font-medium">Installed</h2>
				</div>

				{#if loading}
					<p class="text-sm text-muted-foreground">Loading…</p>
				{:else if installed.length === 0}
					<div class="grid gap-1 rounded-lg border border-dashed p-6 text-center">
						<p class="font-medium">No mods yet</p>
						<p class="text-sm text-muted-foreground">
							Browse the catalogue to find mods for this server.
						</p>
						<Button
							variant="outline"
							class="mt-2 justify-self-center"
							onclick={() => (activeTab = 'browse')}>Browse mods</Button
						>
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
								<details
									class="group"
									open={dependencies.some((m) => m.load_status === 'not_seen')}
								>
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
		</Tabs.Content>
		<Tabs.Content value="browse" class="grid gap-3 data-[state=inactive]:hidden">
			<section class="grid gap-3">
				<div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
					<h2 class="font-medium">{canManage ? 'Add a mod' : 'Catalogue'}</h2>
					<span class="text-sm text-muted-foreground">
						{catalogueMessage ??
							(syncedAt
								? `Catalogue updated ${when(syncedAt)}`
								: 'Catalogue sync status unavailable.')}
					</span>
				</div>

				<!-- Which registry to browse. A row of buttons rather than a dropdown: there are
				     three choices and the current one should be readable without opening
				     anything. Styled to the tab pill above it. -->
				<div
					class="flex w-fit flex-wrap gap-1 rounded-lg bg-muted p-1"
					role="group"
					aria-label="Mod registry"
				>
					{#each modSources as choice (choice.label)}
						<Button
							variant="ghost"
							size="sm"
							aria-pressed={registry === choice.value}
							class={[
								'rounded-md px-3 font-medium',
								registry === choice.value
									? 'bg-background text-foreground shadow-sm'
									: 'text-muted-foreground'
							]}
							onclick={() => (registry = choice.value)}
						>
							{choice.label}
						</Button>
					{/each}
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
							{catalogueMessage ?? 'No packages in this catalogue.'}
						{/if}
					</p>
				{:else}
					<ul class="divide-y rounded-lg border">
						{#each results as mod (`${mod.full_name}:${mod.source}`)}
							{@const installedMod = installedByName.get(mod.full_name)}
							{@const state = modOffer(mod, installedMod)}
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
										<Badge variant="outline" class={sourceBadge[mod.source]}>
											{sourceLabel[mod.source] ?? mod.source}
										</Badge>
										{#if mod.is_deprecated}
											<Badge variant="destructive">deprecated</Badge>
										{/if}
										{#if installedMod}
											<Badge variant="outline" class="h-auto max-w-full whitespace-normal"
												>{state === 'other-source'
													? `Installed from ${sourceLabel[installedMod.source]}`
													: 'installed'}</Badge
											>
										{/if}
									</div>
									{#if mod.description}
										<p class="max-w-prose text-sm text-muted-foreground">{mod.description}</p>
									{/if}
									<p class="text-xs text-muted-foreground tabular-nums">
										<span class={sourceText[mod.source]}>{mod.latest_version}</span>
										· {compact.format(mod.downloads)} downloads
									</p>
								</div>

								{#if canManage}
									<Button
										class="shrink-0"
										variant={state === 'update' ? 'default' : 'outline'}
										size="sm"
										disabled={!canAct ||
											state === 'installed' ||
											state === 'other-source' ||
											resolvingName !== null}
										onclick={() => askToInstall(mod)}
									>
										{#if resolvingName === mod.full_name}
											Checking…
										{:else if state === 'installed' || state === 'other-source'}
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
							onclick={() => void search(query, registry, nextCursor)}
						>
							Show more
						</Button>
					{/if}
				{/if}
			</section>
		</Tabs.Content>
		<Tabs.Content value="players" class="grid gap-3 data-[state=inactive]:hidden">
			{#if exportLoading}
				<p class="text-sm text-muted-foreground">Loading player modpack…</p>
			{:else if exportFailure}
				<Problem error={exportFailure} />
				<Button variant="outline" class="justify-self-start" onclick={readClientExport}
					>Retry loading player modpack</Button
				>
			{:else if clientExport}
				{@const untagged = clientExport.excluded.filter((e) => e.side === 'unknown')}
				<section class="grid gap-3">
					<div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
						<h2 class="font-medium">Mods your players need</h2>
						<span class="text-sm text-muted-foreground">
							{clientExport.mods.length}
							{clientExport.mods.length === 1 ? 'package' : 'packages'}, pinned to the versions this
							server runs
						</span>
					</div>

					<div class="grid gap-3 rounded-lg border p-4">
						{#if clientExport.mods.length === 0}
							<p class="text-sm text-muted-foreground">
								Nothing is labelled for clients yet. Label a mod "Client required" or "Client
								optional" on the Installed tab and it appears here, together with everything it
								depends on.
							</p>
						{:else}
							<ul class="grid gap-1 text-sm">
								{#each clientExport.mods as entry (entry.full_name)}
									<li class="flex flex-wrap items-center gap-2">
										<span class="font-mono text-xs">{entry.full_name}</span>
										<span class="text-xs text-muted-foreground">{entry.version}</span>
										{#if entry.reason === 'dependency'}
											<Badge variant="secondary">dependency</Badge>
										{/if}
									</li>
								{/each}
							</ul>
						{/if}

						{#if untagged.length > 0}
							<Alert.Root>
								<TriangleAlert />
								<Alert.Title>
									{untagged.length}
									{untagged.length === 1 ? 'mod is' : 'mods are'} unlabelled and left out
								</Alert.Title>
								<Alert.Description>
									{untagged.map((e) => e.full_name).join(', ')} — nobody has said whether players need
									these, so the export leaves them out rather than guessing.
								</Alert.Description>
							</Alert.Root>
						{/if}

						{#if clientExport.conflicts.length > 0}
							<Alert.Root variant="destructive">
								<TriangleAlert />
								<Alert.Title>This list cannot be exported yet</Alert.Title>
								<Alert.Description class="grid gap-1">
									{#each clientExport.conflicts as conflict (conflict.full_name + conflict.required_by)}
										<span>
											{conflict.required_by} needs {conflict.full_name}, which is
											{conflict.side === 'server_only'
												? 'labelled server only'
												: 'not in the catalogue'}.
										</span>
									{/each}
								</Alert.Description>
							</Alert.Root>
						{/if}

						<div class="flex flex-wrap items-center gap-3">
							<Button
								variant="outline"
								size="sm"
								href={mods.exportUrl(id)}
								download
								disabled={clientExport.mods.length === 0 || clientExport.conflicts.length > 0}
							>
								<Download />
								Download client list
							</Button>
							<span class="text-xs text-muted-foreground">
								A profile file for r2modman, Gale, or Thunderstore Mod Manager. It carries package
								names and versions only.
							</span>
						</div>
					</div>
				</section>
			{/if}
		</Tabs.Content>
	</Tabs.Root>
</div>

{#snippet installedRow(mod: InstalledMod)}
	{@const newer = installedUpdateTarget(mod)}
	<div class="grid min-w-0 flex-1 gap-1">
		<div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
			<span class="font-medium">{mod.name || mod.full_name}</span>
			{#if mod.namespace}
				<span class="text-sm text-muted-foreground">by {mod.namespace}</span>
			{/if}
			<span class={['text-sm tabular-nums', sourceText[mod.source] ?? 'text-muted-foreground']}>
				{mod.version}
			</span>
			<Badge variant="outline" class={sourceBadge[mod.source]}>
				{sourceLabel[mod.source] ?? mod.source}
			</Badge>
			{#if newer}
				<Badge variant="outline">{newer.latest_version} available</Badge>
				{#if canManage}
					<Button
						size="sm"
						disabled={!canAct || resolvingName !== null}
						onclick={() => askToInstall(newer)}
					>
						<ArrowUpCircle />
						{resolvingName === newer.full_name ? 'Checking…' : 'Update'}
					</Button>
				{/if}
			{/if}
			{#if mod.is_deprecated}
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
		</div>
		<p class="text-sm text-muted-foreground">
			{mod.file_count}
			{mod.file_count === 1 ? 'file' : 'files'} · added {when(mod.installed_at)}
		</p>
	</div>
	{#if canManage}
		<div class="grid shrink-0 gap-1">
			<Label class="sr-only" for={`side-${mod.full_name}`}>Client requirement</Label>
			<Select.Root
				type="single"
				value={mod.side}
				disabled={!canTag}
				onValueChange={(side) => void setSide(mod, side as ModSide)}
			>
				<Select.Trigger id={`side-${mod.full_name}`} class="w-40">
					{taggingName === mod.full_name ? 'Saving…' : sideLabel(mod.side)}
				</Select.Trigger>
				<Select.Content>
					{#each modSides as option (option.value)}
						<Select.Item value={option.value}>{option.label}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
	{:else}
		<Badge variant="secondary">{sideLabel(mod.side)}</Badge>
	{/if}
	{#if canManage}
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
			{@const updating = installedNames.has(pending.target.full_name)}
			{@const changes = pending.nodes.filter((node) => !node.no_op).length}
			<Dialog.Header>
				<Dialog.Title>{updating ? 'Update' : 'Install'} {pending.target.name}?</Dialog.Title>
				<Dialog.Description>
					{changes === 0
						? 'Everything required is already installed. No changes are needed.'
						: changes === 1
							? 'One package will be installed or updated.'
							: `${changes} packages will be installed or updated.`}
				</Dialog.Description>
			</Dialog.Header>
			<ul class="grid max-h-64 gap-2 overflow-y-auto text-sm">
				{#each pending.nodes as node (`${node.full_name}:${node.source}`)}
					<li class="flex flex-wrap items-center gap-2">
						<span class="font-medium">{node.full_name}</span>
						<span class={['tabular-nums', sourceText[node.source] ?? 'text-muted-foreground']}>
							{node.version}
						</span>
						<Badge variant="outline" class={sourceBadge[node.source]}>
							{sourceLabel[node.source] ?? node.source}
						</Badge>
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
				<Button disabled={changes === 0 || !canAct} onclick={installConfirmed}
					>{updating ? 'Update mod' : 'Install mod'}</Button
				>
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
					Remove unused dependencies too
					<span class="text-xs text-muted-foreground">
						Only the dependencies no other mod still needs.
					</span>
				</Label>
			</div>
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (removeOpen = false)}>Cancel</Button>
				<Button variant="destructive" onclick={removeConfirmed}>Remove mod</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
