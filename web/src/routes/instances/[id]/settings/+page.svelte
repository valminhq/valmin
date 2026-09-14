<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import {
		actions,
		instances,
		type GameOptions,
		type Instance,
		type PatchInstance
	} from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import RestartNotice from '$lib/components/restart-notice.svelte';
	import StateBadge from '$lib/components/state-badge.svelte';
	import WorldImport from '$lib/components/world-import.svelte';
	import { manifest } from '$lib/api/manifest';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Download from '@lucide/svelte/icons/download';
	import Lock from '@lucide/svelte/icons/lock';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');
	// The whole link, so an operator can copy it out of the panel and send it to a friend.
	const statusURL = $derived(`${page.url.origin}/status/${id}`);

	let instance = $state<Instance | null>(null);
	let options = $state<GameOptions | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);
	let saving = $state(false);
	let confirming = $state(false);
	let exporting = $state(false);

	let serverName = $state('');
	let password = $state('');
	let isPublic = $state(false);
	let statusPublished = $state(false);
	let crossplay = $state(false);
	let preset = $state('');
	let modifiers = $state<Record<string, string>>({});
	let memLimitMB = $state<number | undefined>();
	let cpuLimit = $state<number | undefined>();

	const allowed = $derived(session.allowed(id));
	const canEdit = $derived(allowed.includes(actions.settings));
	// The manifest is settings plus mods plus config, so the button appears only for someone
	// who holds all three — the same conjunction the daemon checks.
	const canExportManifest = $derived(
		allowed.includes(actions.settings) &&
			allowed.includes(actions.modsList) &&
			allowed.includes(actions.configRead)
	);
	const canEditLimits = $derived(allowed.includes(actions.limits));
	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const minPassword = $derived(options?.min_password_length ?? 5);
	const minMemory = $derived(options?.min_memory_limit_mb);

	$effect(() => {
		void load();
	});

	/** Built in the browser rather than served as a file: the endpoint answers JSON, and a
	 * blob keeps the one download from needing a second representation on the daemon. */
	async function downloadManifest() {
		exporting = true;
		failure = null;
		try {
			const doc = await manifest.export(id);
			const url = URL.createObjectURL(
				new Blob([JSON.stringify(doc, null, 2)], { type: 'application/json' })
			);
			const a = document.createElement('a');
			a.href = url;
			a.download = `${doc.name || 'instance'}.valmin.json`;
			a.click();
			URL.revokeObjectURL(url);
		} catch (err) {
			failure = err;
		} finally {
			exporting = false;
		}
	}

	async function load() {
		try {
			const [inst, opts] = await Promise.all([instances.get(id), instances.options()]);
			options = opts;
			adopt(inst);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	/** Takes the daemon's row as the form's new baseline, so the screen never shows a value it
	 * merely sent (F4). */
	function adopt(row: Instance) {
		instance = row;
		serverName = row.server_name;
		password = '';
		isPublic = row.public;
		statusPublished = row.status_published;
		crossplay = row.crossplay;
		preset = row.preset ?? '';
		modifiers = decodeModifiers(row.modifiers);
		memLimitMB = row.mem_limit_mb;
		cpuLimit = row.cpu_limit ?? undefined;
	}

	/** Modifiers are stored as a JSON object in one column (`04 §2`). Anything unparseable is
	 * treated as no modifiers rather than as a broken screen. */
	function decodeModifiers(raw: string | undefined): Record<string, string> {
		if (!raw) return {};
		try {
			const parsed: unknown = JSON.parse(raw);
			return parsed && typeof parsed === 'object' ? (parsed as Record<string, string>) : {};
		} catch {
			return {};
		}
	}

	/** Blank values are absent values, and order is not a change. */
	function normalise(m: Record<string, string>): string {
		return JSON.stringify(
			Object.entries(m)
				.map(([key, value]) => [key, value.trim()] as const)
				.filter(([, value]) => value !== '')
				.sort(([a], [b]) => a.localeCompare(b))
		);
	}

	const setModifiers = $derived(
		Object.fromEntries(
			Object.entries(modifiers)
				.map(([key, value]) => [key, value.trim()] as const)
				.filter(([, value]) => value !== '')
		)
	);

	const changed = $derived.by(() => {
		if (!instance) return [];
		const fields: string[] = [];
		if (serverName.trim() !== instance.server_name) fields.push('server_name');
		if (password !== '') fields.push('password');
		if (isPublic !== instance.public) fields.push('public');
		if (statusPublished !== instance.status_published) fields.push('status_published');
		if (crossplay !== instance.crossplay) fields.push('crossplay');
		if (preset !== (instance.preset ?? '')) fields.push('preset');
		if (normalise(modifiers) !== normalise(decodeModifiers(instance.modifiers))) {
			fields.push('modifiers');
		}
		if (memLimitMB !== instance.mem_limit_mb) fields.push('mem_limit_mb');
		if ((cpuLimit ?? null) !== instance.cpu_limit) fields.push('cpu_limit');
		return fields;
	});

	// `03 §1.3`'s three rules, client-side as a courtesy: the daemon checks them against the
	// merged row and again at container creation (G2). Rule 2 is only partly checkable here,
	// since an unchanged password is not on this page.
	const localProblems = $derived.by(() => {
		const problems: Record<string, string> = {};
		const world = instance?.world_name ?? '';
		if (password && password.length < minPassword) {
			problems.password = `At least ${minPassword} characters.`;
		}
		if (password && (serverName.includes(password) || world.includes(password))) {
			problems.password = 'The password must not appear inside the server or world name.';
		}
		if (serverName && serverName === world) {
			problems.server_name = 'The server name must differ from the world name.';
		}
		if (serverName.trim() === '') problems.server_name = 'Players need a name to find.';
		if (memLimitMB === undefined || !Number.isInteger(memLimitMB)) {
			problems.mem_limit_mb = 'Enter a whole number of megabytes.';
		} else if (minMemory !== undefined && memLimitMB < minMemory) {
			problems.mem_limit_mb = `Use at least ${minMemory} MB.`;
		}
		if (cpuLimit !== undefined && (!Number.isFinite(cpuLimit) || cpuLimit <= 0)) {
			problems.cpu_limit = 'Use a number greater than 0, or leave this blank.';
		}
		return problems;
	});

	function problem(field: string): string | undefined {
		return localProblems[field] ?? apiError?.field(field);
	}

	const maySave = $derived(
		changed.every((field) =>
			field === 'mem_limit_mb' || field === 'cpu_limit' ? canEditLimits : canEdit
		)
	);
	const ready = $derived(
		maySave && changed.length > 0 && Object.keys(localProblems).length === 0 && !saving
	);

	/** A new password locks every player out until they are told it, so it is the one field here
	 * that asks twice (F5). */
	function submit() {
		if (changed.includes('password')) confirming = true;
		else void save();
	}

	async function save() {
		saving = true;
		failure = null;
		const body: PatchInstance = {};
		if (changed.includes('server_name')) body.server_name = serverName.trim();
		if (changed.includes('password')) body.password = password;
		if (changed.includes('public')) body.public = isPublic;
		if (changed.includes('status_published')) body.status_published = statusPublished;
		if (changed.includes('crossplay')) body.crossplay = crossplay;
		if (changed.includes('preset')) body.preset = preset;
		if (changed.includes('modifiers')) body.modifiers = setModifiers;
		if (changed.includes('mem_limit_mb') && memLimitMB !== undefined) {
			body.mem_limit_mb = memLimitMB;
		}
		if (changed.includes('cpu_limit')) body.cpu_limit = cpuLimit ?? null;
		try {
			adopt(await instances.patch(id, body));
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
</script>

<div class="mx-auto grid max-w-6xl gap-6 p-6">
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
				<h1 class="text-2xl font-semibold tracking-tight">Server settings</h1>
				<p class="text-sm text-muted-foreground">
					Manage identity, connections, gameplay, and resource limits. Saved changes take effect on
					the next start.
				</p>
			</div>
			{#if instance}
				<StateBadge state={instance.state} restartRequired={instance.restart_required} />
			{/if}
		</div>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else}
		{#if instance.restart_required}
			<RestartNotice />
		{/if}

		{#if !canEdit && !canEditLimits}
			<p class="text-sm text-muted-foreground" data-testid="settings-blocked">
				You can see these settings but not change them.
			</p>
		{/if}

		<div class="grid items-start gap-6 lg:grid-cols-2">
			<Card.Root>
				<Card.Header>
					<Card.Title>Server identity</Card.Title>
					<Card.Description
						>The name players see in the server browser and the password they use to join.</Card.Description
					>
				</Card.Header>
				<Card.Content class="grid gap-4">
					<div class="grid gap-2">
						<Label for="server_name">Server name</Label>
						<Input id="server_name" bind:value={serverName} disabled={!canEdit} />
						{#if problem('server_name')}
							<p class="text-sm text-destructive">{problem('server_name')}</p>
						{/if}
					</div>

					<!--
					Shown read-only rather than omitted (Q48): `-world` names the save file basename, so
					renaming it moves the `.db`/`.fwl` pair and needs a job rather than a column write
					(`03 §1.3`, `03 §4.1`, ADR-077).
				-->
					<div class="grid gap-2">
						<Label for="world_name">World name</Label>
						<div class="relative">
							<Input id="world_name" value={instance.world_name} readonly disabled />
							<Lock
								class="pointer-events-none absolute top-1/2 right-3 size-4 -translate-y-1/2 text-muted-foreground"
							/>
						</div>
						<p class="text-xs text-muted-foreground">
							This is the name of the save file on disk, so changing it moves the world rather than
							a setting. The panel cannot do that yet.
						</p>
					</div>

					<div class="grid gap-2">
						<Label for="password">Server password</Label>
						<Input
							id="password"
							type="password"
							autocomplete="new-password"
							placeholder="Unchanged"
							disabled={!canEdit}
							bind:value={password}
						/>
						<p class="text-xs text-muted-foreground">
							Leave this blank to keep the current one. The panel shows the current password on the
							server's own page.
						</p>
						{#if problem('password')}
							<p class="text-sm text-destructive">{problem('password')}</p>
						{/if}
					</div>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Visibility and connections</Card.Title>
					<Card.Description>
						Control community listing, public status, and cross-platform connections.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-4">
					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="public">List publicly</Label>
							<p class="text-xs text-muted-foreground">
								Show this server in the community browser.
							</p>
						</div>
						<Switch id="public" disabled={!canEdit} bind:checked={isPublic} />
					</div>

					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="status_published">Public status page</Label>
							<p class="text-xs text-muted-foreground">
								Lets anyone with the link see this server's name, whether it is up, and how many
								players are on it — without signing in. Off unless you turn it on.
							</p>
							{#if instance.status_published}
								<a
									class="text-xs underline underline-offset-4"
									href={resolve('/status/[id]', { id })}
									target="_blank"
									rel="noreferrer">{statusURL}</a
								>
							{/if}
						</div>
						<Switch id="status_published" disabled={!canEdit} bind:checked={statusPublished} />
					</div>

					<div class="flex items-center justify-between gap-4">
						<div class="grid gap-1">
							<Label for="crossplay">Crossplay</Label>
							<p class="text-xs text-muted-foreground">
								Lets players on other platforms find and join this server.
							</p>
						</div>
						<Switch id="crossplay" disabled={!canEdit} bind:checked={crossplay} />
					</div>

					<!--
					`03 §1.4` rule 5, with the list from the daemon so the panel cannot quietly stop
					warning (Q6). The join code is rendered above, not here.
				-->
					{#if crossplay && options}
						<div class="grid gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3">
							<p class="flex items-center gap-2 text-sm font-medium">
								<TriangleAlert class="size-4" />
								Untested combinations
							</p>
							<p class="text-sm text-muted-foreground">
								Compatibility has not been tested for these combinations:
							</p>
							<ul class="list-inside list-disc text-sm text-muted-foreground">
								{#each options.crossplay_untested as combination (combination)}
									<li>{combination}</li>
								{/each}
							</ul>
							<p class="text-sm text-muted-foreground">
								Turning this off and restarting undoes it. The server is rebuilt on the next start
								either way, and keeps the identity it was given when it was created.
							</p>
						</div>
					{/if}
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Gameplay</Card.Title>
					<Card.Description>
						World preset and individual modifiers for combat, raids, and other game rules.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-4">
					<!--
					What a changed preset does to an existing world is unmeasured (Q49, E8); `03 §1.3.1`
					measured only which names the parser accepts. The screen claims nothing either way.
				-->
					<p
						class="flex items-start gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3 text-sm text-muted-foreground"
					>
						<TriangleAlert class="mt-0.5 size-4 shrink-0" />
						<span>
							The effects on existing worlds have not been verified. Back up your world before
							changing these settings.
						</span>
					</p>

					<div class="grid gap-2">
						<Label for="preset">World preset</Label>
						<Select.Root type="single" disabled={!canEdit} bind:value={preset}>
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
							The list was built by feeding candidates to the real parser, which confirms what
							it is given and cannot enumerate the rest (`03 §1.3.1`).
						-->
							<p class="text-xs text-muted-foreground">
								These presets were tested with game build {options.build}. Other presets may exist.
							</p>
						{/if}
						{#if problem('preset')}<p class="text-sm text-destructive">{problem('preset')}</p>{/if}
					</div>

					{#if options}
						{#if !options.modifier_values_measured}
							<!--
							The five axes are measured (`03 §1.3`); their legal values are not, since the
							`.fwl`'s stored form is not proven to be the command-line grammar (E8).
						-->
							<p class="text-xs text-muted-foreground">
								The game supports these modifiers, but their accepted values have not been verified.
								Leave a field blank unless you know which value to use.
							</p>
						{/if}
						{#each options.modifier_keys as key (key)}
							<div class="grid gap-2">
								<Label for={`modifier-${key}`}>{key}</Label>
								<Input
									id={`modifier-${key}`}
									value={modifiers[key] ?? ''}
									disabled={!canEdit}
									oninput={(event) => (modifiers[key] = event.currentTarget.value)}
								/>
							</div>
						{/each}
						{#if problem('modifiers')}
							<p class="text-sm text-destructive">{problem('modifiers')}</p>
						{/if}
					{/if}
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Resource limits</Card.Title>
					<Card.Description>
						The panel uses these limits when it builds the container on the next start. Saving does
						not change the running container.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-4 sm:grid-cols-2" data-testid="limit-controls">
					<div class="grid content-start gap-2">
						<Label for="mem_limit_mb">Memory limit (MB)</Label>
						<Input
							id="mem_limit_mb"
							type="number"
							min={minMemory}
							step="256"
							disabled={!canEditLimits}
							aria-invalid={problem('mem_limit_mb') ? 'true' : undefined}
							aria-describedby="mem_limit_mb-help mem_limit_mb-error"
							bind:value={memLimitMB}
						/>
						<p id="mem_limit_mb-help" class="text-xs text-muted-foreground">
							{#if minMemory !== undefined}
								At least {minMemory} MB. Use generous headroom: exhausting the limit can interrupt a save.
							{:else}
								Use generous headroom: exhausting the limit can interrupt a save.
							{/if}
						</p>
						{#if problem('mem_limit_mb')}
							<p id="mem_limit_mb-error" class="text-sm text-destructive" role="alert">
								{problem('mem_limit_mb')}
							</p>
						{/if}
					</div>

					<div class="grid content-start gap-2">
						<Label for="cpu_limit">CPU limit (cores)</Label>
						<Input
							id="cpu_limit"
							type="number"
							min="0.01"
							step="0.25"
							placeholder="No quota"
							disabled={!canEditLimits}
							aria-invalid={problem('cpu_limit') ? 'true' : undefined}
							aria-describedby="cpu_limit-help cpu_limit-error"
							bind:value={cpuLimit}
						/>
						<p id="cpu_limit-help" class="text-xs text-muted-foreground">
							Leave blank for no CPU quota. A tight quota can hurt a simulation that depends heavily
							on one core.
						</p>
						{#if problem('cpu_limit')}
							<p id="cpu_limit-error" class="text-sm text-destructive" role="alert">
								{problem('cpu_limit')}
							</p>
						{/if}
					</div>

					{#if !canEditLimits}
						<p class="text-xs text-muted-foreground sm:col-span-2">
							Resource limits are visible here but require the separate limit-management capability
							to change.
						</p>
					{/if}
				</Card.Content>
			</Card.Root>
		</div>

		{#if canExportManifest}
			<Card.Root>
				<Card.Header>
					<Card.Title>This server's definition</Card.Title>
					<Card.Description>
						Launch settings, the mods pinned to their installed versions, and every config file —
						one document, importable as a new server.
					</Card.Description>
				</Card.Header>
				<Card.Content class="flex flex-wrap items-center gap-3">
					<Button variant="outline" size="sm" disabled={exporting} onclick={downloadManifest}>
						<Download />
						{exporting ? 'Preparing…' : 'Download server definition'}
					</Button>
					<p class="text-xs text-muted-foreground">
						It carries no password and no world. It does carry your config files as they are on
						disk, and a mod's config can hold a key or a webhook — read it before you share it.
					</p>
				</Card.Content>
			</Card.Root>
		{/if}

		<!-- Its own capability and its own confirmation: this replaces world data, and the save
		     bar below does not apply to it. -->
		<WorldImport {instance} />

		{#if canEdit || canEditLimits}
			<div
				class="sticky bottom-0 -mx-6 flex flex-wrap items-center justify-between gap-3 border-t bg-background/95 px-6 py-3 backdrop-blur"
			>
				<p class="text-sm text-muted-foreground">
					{changed.length === 0
						? 'Nothing to save.'
						: `${changed.length} ${changed.length === 1 ? 'setting' : 'settings'} changed. The server picks them up on its next start.`}
				</p>
				<div class="flex gap-2">
					<Button
						variant="ghost"
						size="sm"
						disabled={changed.length === 0 || saving}
						onclick={() => instance && adopt(instance)}
					>
						Discard changes
					</Button>
					<Button size="sm" disabled={!ready} onclick={submit}>
						{saving ? 'Saving…' : 'Save changes'}
					</Button>
				</div>
			</div>
		{/if}
	{/if}
</div>

<Dialog.Root bind:open={confirming}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Change the server password?</Dialog.Title>
			<Dialog.Description>
				Everyone who plays here needs the new password before they can get back in, and the panel
				cannot tell them. Players connected right now stay connected until the server restarts.
			</Dialog.Description>
		</Dialog.Header>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (confirming = false)}>Cancel</Button>
			<Button
				onclick={() => {
					confirming = false;
					void save();
				}}
			>
				Save changes and password
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
