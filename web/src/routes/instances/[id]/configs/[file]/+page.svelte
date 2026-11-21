<script lang="ts">
	import { formatInstant } from '#lib/api/schedules.js';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError } from '#lib/api/errors.js';
	import { actions, instances, stateSentence, type Instance } from '#lib/api/instances.js';
	import {
		configs,
		fieldOf,
		type ConfigCopy,
		type ConfigRevision,
		type ConfigSchema,
		type ConfigValue
	} from '#lib/api/configs.js';
	import { hasSettings, newerModVersion, revisionLabel } from '#lib/config-files.js';
	import { session } from '#lib/state/session.svelte.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { topics, type ServerMessage } from '#lib/socket/messages.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import * as Dialog from '#lib/components/ui/dialog/index.js';
	import Problem from '#lib/components/problem.svelte';
	import RestartNotice from '#lib/components/restart-notice.svelte';
	import ConfigSetting from '#lib/components/config-setting.svelte';
	import ConfigRaw from '#lib/components/config-raw.svelte';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Search from '@lucide/svelte/icons/search';
	import History from '@lucide/svelte/icons/history';

	const id = $derived(page.params.id ?? '');
	const file = $derived(page.params.file ?? '');
	/** A file with no settings (YAML, JSON, text) has only the raw editor. */
	const textOnly = $derived(!hasSettings(file));

	let instance = $state<Instance | null>(null);
	let schema = $state<ConfigSchema | null>(null);
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let query = $state('');
	let reviewing = $state(false);
	let loadRequest = 0;

	/**
	 * What the form holds, and what the file held when it was read. Two maps rather than a list
	 * of pending changes: a setting is changed exactly when the two disagree, so editing a value
	 * back to what it was stops counting as one.
	 */
	let edits = $state<Record<string, ConfigValue>>({});
	let original = $state<Record<string, ConfigValue>>({});

	/**
	 * The file's kept versions, newest first, and what the form and the raw view compare against:
	 * a version's id, the mod's declared defaults, nothing, or empty until the first load picks.
	 * `compared` is the chosen version's settings once read.
	 */
	let history = $state<ConfigRevision[]>([]);
	let compare = $state<string>('');
	let compared = $state<ConfigCopy | null>(null);

	const chosen = $derived(history.find((rev) => rev.id === compare) ?? null);
	/** The newest mod-written version when the chosen one is older, so loading it may hand the
	 * mod a file it no longer expects. */
	const predates = $derived(chosen ? newerModVersion(history, chosen.id) : null);
	/** The newest version, when a mod update wrote it over a file that held edits. */
	const replacedBy = $derived(
		history[0]?.source === 'mod' && history[0].replaced_edits ? history[0] : null
	);
	const refValues = $derived(
		compare === 'defaults' ? defaultsOf(schema) : compared ? valuesOf(compared) : {}
	);
	const comparing = $derived(compare === 'defaults' ? schema !== null : compared !== null);

	/** Settings whose value in the reference differs from the file. Compared against the file
	 * rather than the pending edits, so the list does not shift while the operator types. Keys
	 * the file no longer has are dropped, since restoring one would patch nothing. */
	const differences = $derived(
		Object.keys(refValues).filter(
			(field) => field in original && String(refValues[field]) !== String(original[field])
		)
	);

	const allowed = $derived(session.allowed(id));
	const canEdit = $derived(allowed.includes(actions.configEdit));
	const canRaw = $derived(allowed.includes(actions.configRaw));
	/** Whether this member can change the file at all: a text file only through the raw editor. */
	const canWrite = $derived(textOnly ? canRaw : canEdit);

	/**
	 * Which editor is on screen, and whether the raw one has ever been opened. Both stay mounted
	 * once shown, so switching back keeps what was typed. The raw tab is unavailable while the
	 * form has pending changes: they edit the same bytes.
	 */
	let tab = $state<'form' | 'raw'>('form');
	let rawOpened = $state(false);
	const onRaw = $derived(textOnly || tab === 'raw');

	/** Why editing is unavailable, or null when it is available (B11). The daemon refuses a write
	 * to a server that is neither stopped nor running; this makes the refusal legible before the
	 * click. */
	const blocked = $derived.by(() => {
		if (!instance) return 'Loading this server.';
		if (instance.state !== 'stopped' && instance.state !== 'running') {
			return `${stateSentence(instance.state)} Settings change when it is stopped or running.`;
		}
		return null;
	});
	const editable = $derived(canEdit && blocked === null && !saving);

	const changed = $derived(Object.keys(edits).filter((f) => edits[f] !== original[f]));
	unsaved(() => changed.length > 0);

	const apiError = $derived(failure instanceof ApiError ? failure : null);
	function problem(field: string): string {
		return apiError?.field(field) ?? '';
	}

	const needle = $derived(query.trim().toLowerCase());
	/** The file as it will be shown: sections keep their order and names, and one with nothing
	 * matching the filter drops out. */
	const shown = $derived(
		(schema?.sections ?? [])
			.map((section) => ({
				...section,
				settings: section.settings.filter(
					(s) =>
						!needle ||
						s.key.toLowerCase().includes(needle) ||
						s.description.toLowerCase().includes(needle) ||
						section.name.toLowerCase().includes(needle)
				)
			}))
			.filter((section) => section.settings.length > 0)
	);

	// Subscribe, then fetch (G3, `14 §7.2`). The state topic is what tells this page the server
	// was started from another tab, which disables the form instead of failing the save.
	$effect(() => {
		const off = socket.subscribe(topics.state(id), (m: ServerMessage) => {
			if (m.type !== 'state' || !instance) return;
			instance = {
				...instance,
				state: m.state,
				restart_required: m.restart_required,
				pending_restart: m.pending_restart
			};
		});
		void load(id, file);
		return off;
	});

	// A reconnect re-reads the form only when nothing is unsaved; otherwise just the server row,
	// so typed changes survive a dropped connection.
	$effect(() =>
		socket.onConnected(() => {
			if (changed.length === 0) void load(id, file);
			else void reloadInstance(id);
		})
	);

	async function reloadInstance(targetID: string) {
		try {
			instance = await instances.get(targetID);
		} catch (err) {
			failure = err;
		}
	}

	// The chosen version's settings, read when the choice changes. A text file is compared only
	// in the raw view, which reads the bytes itself.
	$effect(() => {
		const rev = compare;
		compared = null;
		if (textOnly || rev === '' || rev === 'off' || rev === 'defaults') return;
		let current = true;
		configs
			.revision(id, file, rev)
			.then((read) => {
				if (current) compared = read;
			})
			.catch(() => {});
		return () => {
			current = false;
		};
	});

	async function load(targetID: string, targetFile: string) {
		const request = ++loadRequest;
		loading = true;
		try {
			const [nextInstance, nextSchema, nextHistory] = await Promise.all([
				instances.get(targetID),
				hasSettings(targetFile) ? configs.read(targetID, targetFile) : null,
				configs
					.history(targetID, targetFile)
					.then((read) => read.items)
					.catch(() => [])
			]);
			if (request !== loadRequest) return;
			instance = nextInstance;
			if (nextSchema) take(nextSchema);
			history = nextHistory;
			compare = pickCompare(nextHistory, compare, !hasSettings(targetFile));
			failure = null;
		} catch (err) {
			if (request !== loadRequest) return;
			failure = err;
		} finally {
			if (request === loadRequest) loading = false;
		}
	}

	/**
	 * What to compare against after a load: the current choice while it still applies, otherwise
	 * the version a mod update replaced, otherwise the baseline. A file with no history compares
	 * against nothing until one is chosen.
	 */
	function pickCompare(items: ConfigRevision[], current: string, textFile: boolean): string {
		if (current === 'off' || items.some((rev) => rev.id === current)) return current;
		if (current === 'defaults' && !textFile) return current;
		if (items[0]?.source === 'mod' && items[0].replaced_edits && items[1]) return items[1].id;
		return items.find((rev) => rev.baseline)?.id ?? 'off';
	}

	/** Each setting's declared default, keyed like the form. A setting the file declared no
	 * metadata for has no known default and is left out. */
	function defaultsOf(read: ConfigSchema | null): Record<string, ConfigValue> {
		const values: Record<string, ConfigValue> = {};
		for (const section of read?.sections ?? []) {
			for (const setting of section.settings) {
				if (setting.type !== '') values[fieldOf(section.name, setting.key)] = setting.default;
			}
		}
		return values;
	}

	function valuesOf(read: ConfigSchema): Record<string, ConfigValue> {
		const values: Record<string, ConfigValue> = {};
		for (const section of read.sections) {
			for (const setting of section.settings)
				values[fieldOf(section.name, setting.key)] = setting.current;
		}
		return values;
	}

	function take(read: ConfigSchema) {
		schema = read;
		const values = valuesOf(read);
		original = values;
		edits = { ...values };
	}

	function discard() {
		edits = { ...original };
		failure = null;
	}

	/** The form does not predict the save: what the file holds afterwards is read back from the
	 * daemon rather than assumed (F4). */
	async function saveConfirmed() {
		reviewing = false;
		saving = true;
		failure = null;
		try {
			const body: Record<string, ConfigValue> = {};
			for (const field of changed) body[field] = edits[field];
			await configs.patch(id, file, body);
			await load(id, file);
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	/** Keyed by position, not name: a hand-edited file may write the same section header twice,
	 * and the daemon reports both in file order. */
	function anchor(index: number): string {
		return `section-${index}`;
	}

	function when(timestamp: string | undefined): string {
		const date = new Date(timestamp ?? '');
		return timestamp && !Number.isNaN(date.getTime()) ? formatInstant(date) : '';
	}

	/** How the chosen comparison is named in the button that loads from it. */
	const chosenLabel = $derived(
		compare === 'defaults' ? "the mod's defaults" : chosen ? revisionLabel(chosen) : ''
	);

	/** Folds the reference's values into the pending edits. It writes nothing on its own: the
	 * operator confirms in the same dialog every other change goes through (F4). */
	function restoreAll() {
		const restored: Record<string, ConfigValue> = {};
		for (const field of differences) restored[field] = refValues[field];
		edits = { ...edits, ...restored };
	}

	function show(value: ConfigValue): string {
		if (typeof value === 'boolean') return value ? 'on' : 'off';
		return String(value) || 'empty';
	}
</script>

<div class="grid gap-6">
	<header class="grid gap-3">
		<Button
			variant="ghost"
			size="sm"
			class="justify-self-start"
			href={resolve('/instances/[id]/configs', { id })}
		>
			<ArrowLeft />
			Mod configuration
		</Button>
		<div class="grid gap-1">
			<h2 class="font-mono text-2xl font-semibold tracking-tight">{file}</h2>
			<p class="text-sm text-muted-foreground">
				{textOnly ? 'Edited as text' : schema?.plugin || 'No plugin named in this file'}
			</p>
		</div>
	</header>

	{#if replacedBy}
		<p class="text-sm" data-testid="config-replaced">
			{revisionLabel(replacedBy)} replaced this file while it held edits. The version it replaced is kept
			below.
		</p>
	{/if}

	<!--
		Offered for a file with history, and for any .cfg against its declared defaults. The
		comparison is by setting rather than by line, so it survives a plugin rewriting the file
		around the values.
	-->
	{#if history.length > 0 || !textOnly}
		<div class="grid gap-3 rounded-md border p-4">
			<div class="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
				<label class="text-muted-foreground" for="config-compare">Compare with</label>
				<NativeSelect id="config-compare" class="max-w-full sm:max-w-96" bind:value={compare}>
					{#each history as rev (rev.id)}
						<option value={rev.id}>{revisionLabel(rev)} · {when(rev.created_at)}</option>
					{/each}
					{#if !textOnly}
						<option value="defaults">The mod's defaults</option>
					{/if}
					<option value="off">Nothing</option>
				</NativeSelect>
			</div>

			{#if predates}
				<p class="text-sm text-muted-foreground" data-testid="config-predates">
					{textOnly
						? `This version is from before ${revisionLabel(predates)}. The mod may not read it.`
						: `This version is from before ${revisionLabel(predates)}. Settings the file no longer has are skipped when loading.`}
				</p>
			{/if}

			{#if comparing && !textOnly}
				{#if differences.length === 0}
					<p class="text-sm text-muted-foreground">Nothing differs from {chosenLabel}.</p>
				{:else}
					<details class="text-sm">
						<summary class="cursor-pointer text-muted-foreground">
							{differences.length}
							{differences.length === 1 ? 'setting differs' : 'settings differ'} from {chosenLabel}
						</summary>
						<ul class="mt-3 grid max-h-64 gap-3 overflow-y-auto">
							{#each differences as field (field)}
								<li class="grid gap-0.5">
									<span class="font-mono text-xs text-muted-foreground">{field}</span>
									<span class="flex flex-wrap items-baseline gap-2">
										<span class="text-muted-foreground line-through">{show(refValues[field])}</span>
										<span class="font-medium">{show(original[field])}</span>
									</span>
								</li>
							{/each}
						</ul>
					</details>

					<div class="flex flex-wrap gap-2">
						{#if canEdit}
							<!-- Puts the values back into the form, where the usual confirmation applies. -->
							<Button variant="outline" size="sm" disabled={!editable} onclick={restoreAll}>
								<History />
								Load {differences.length}
								{differences.length === 1 ? 'setting' : 'settings'} from {chosenLabel}
							</Button>
						{/if}
						{#if canRaw && chosen}
							<!-- This list is by setting; a changed comment or a reshaped file is visible
							     only line by line, in the raw view. -->
							<Button
								variant="ghost"
								size="sm"
								onclick={() => {
									rawOpened = true;
									tab = 'raw';
								}}
							>
								Compare as text
							</Button>
						{/if}
					</div>
				{/if}
			{/if}
		</div>
	{/if}

	<Problem error={failure} />
	{#if apiError?.fields.length}
		<p class="-mt-4 text-sm text-muted-foreground">
			Nothing was written. Fix the settings marked below and save again.
		</p>
	{/if}

	{#if textOnly && !canRaw}
		<p class="text-sm text-muted-foreground">
			This file has no settings form. Opening it as text needs permission to edit raw config files.
		</p>
	{:else if canWrite && blocked}
		<p class="text-sm text-muted-foreground" data-testid="config-actions-blocked">{blocked}</p>
	{:else if canWrite && instance?.state === 'running'}
		<p class="text-sm text-muted-foreground">
			This server is running. Saved settings take effect when it restarts; some mods pick them up
			sooner.
		</p>
	{:else if !canWrite && !loading}
		<p class="text-sm text-muted-foreground">You can read these settings but not change them.</p>
	{/if}

	{#if instance}
		<RestartNotice {instance} canRestart={session.allowed(id).includes(actions.restart)} />
	{/if}

	{#if canRaw && !textOnly}
		<!-- Two buttons rather than a tabs component, which the panel does not have. -->
		<div class="flex gap-1 border-b" aria-label="Editor">
			<button
				type="button"
				class="-mb-px border-b-2 px-3 py-2 text-sm {tab === 'form'
					? 'border-b-foreground font-medium'
					: 'border-b-transparent text-muted-foreground hover:text-foreground'}"
				aria-pressed={tab === 'form'}
				onclick={() => (tab = 'form')}
			>
				Settings
			</button>
			<button
				type="button"
				class="-mb-px border-b-2 px-3 py-2 text-sm disabled:opacity-50 {tab === 'raw'
					? 'border-b-foreground font-medium'
					: 'border-b-transparent text-muted-foreground hover:text-foreground enabled:hover:text-foreground'}"
				aria-pressed={tab === 'raw'}
				disabled={changed.length > 0}
				onclick={() => {
					rawOpened = true;
					tab = 'raw';
				}}
			>
				Raw text
			</button>
		</div>
		{#if changed.length > 0}
			<p class="-mt-4 text-sm text-muted-foreground">
				Save or discard your changes to edit this file as text.
			</p>
		{/if}
	{/if}

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !textOnly}
		<div class="grid gap-6" class:hidden={onRaw}>
			<div class="relative">
				<Search
					class="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
				/>
				<Input
					bind:value={query}
					class="pl-9"
					placeholder="Filter settings"
					aria-label="Filter settings"
				/>
			</div>

			<div class="grid gap-8 md:grid-cols-[12rem_minmax(0,1fr)] md:gap-10">
				<!-- The file's own sections, named by their headers as written, are the way through a
			     `.cfg` of a hundred settings. -->
				<nav class="hidden self-start md:sticky md:top-6 md:block">
					<ul class="grid gap-1 border-l">
						{#each shown as section, i (i)}
							<li>
								<a
									class="-ml-px block truncate border-l border-transparent py-1 pl-3 font-mono text-xs text-muted-foreground hover:border-l-foreground hover:text-foreground"
									href="#{anchor(i)}"
								>
									{section.name || 'top of file'}
								</a>
							</li>
						{/each}
					</ul>
				</nav>

				<div class="grid gap-8">
					{#each shown as section, i (i)}
						<section id={anchor(i)} class="grid scroll-mt-6 gap-1">
							<h2 class="font-mono text-sm font-semibold">
								[{section.name || 'top of file'}]
							</h2>
							<div class="divide-y">
								{#each section.settings as setting (setting.key)}
									{@const field = fieldOf(section.name, setting.key)}
									<ConfigSetting
										{setting}
										{field}
										bind:value={edits[field]}
										reference={refValues[field]}
										changed={edits[field] !== original[field]}
										disabled={!editable}
										problem={problem(field)}
									/>
								{/each}
							</div>
						</section>
					{:else}
						<p class="text-sm text-muted-foreground">
							Nothing matches “{query}”. Try a shorter word.
						</p>
					{/each}
				</div>
			</div>
		</div>
	{/if}

	{#if canRaw && (rawOpened || textOnly)}
		<div class:hidden={!onRaw}>
			<ConfigRaw
				{id}
				{file}
				revision={chosen?.id ?? null}
				editable={canRaw && blocked === null}
				onsaved={() => void load(id, file)}
			/>
		</div>
	{/if}
</div>

{#if tab === 'form' && changed.length > 0}
	<!-- What is about to be written, before it is written. -->
	<div class="sticky bottom-0 -mx-4 border-t bg-background/95 px-4 backdrop-blur sm:-mx-6 sm:px-6">
		<div class="flex flex-wrap items-center justify-between gap-3 py-3">
			<p class="text-sm">
				{changed.length}
				{changed.length === 1 ? 'setting' : 'settings'} changed
			</p>
			<div class="flex gap-2">
				<Button variant="ghost" size="sm" onclick={discard} disabled={saving}
					>Discard changes</Button
				>
				<Button size="sm" onclick={() => (reviewing = true)} disabled={!editable}>
					Review changes
				</Button>
			</div>
		</div>
	</div>
{/if}

<!--
	The diff, before anything is written. Nothing sends a patch except this dialog's confirm.
-->
<Dialog.Root bind:open={reviewing}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title
				>Save {changed.length} {changed.length === 1 ? 'change' : 'changes'}?</Dialog.Title
			>
			<Dialog.Description>
				The rest of the file is untouched, and the version before this save is kept in its history.
			</Dialog.Description>
		</Dialog.Header>
		<ul class="grid max-h-64 gap-3 overflow-y-auto text-sm">
			{#each changed as field (field)}
				<li class="grid gap-0.5">
					<span class="font-mono text-xs text-muted-foreground">{field}</span>
					<span class="flex flex-wrap items-baseline gap-2">
						<span class="text-muted-foreground line-through">{show(original[field])}</span>
						<span class="font-medium">{show(edits[field])}</span>
					</span>
				</li>
			{/each}
		</ul>
		<Dialog.Footer>
			<Button variant="outline" onclick={() => (reviewing = false)}>Cancel</Button>
			<Button onclick={saveConfirmed}>Save changes</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
