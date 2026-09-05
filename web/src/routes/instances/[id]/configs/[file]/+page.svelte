<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import {
		configs,
		copies,
		fieldOf,
		type ConfigCopy,
		type ConfigCopyName,
		type ConfigSchema,
		type ConfigValue
	} from '$lib/api/configs';
	import { session } from '$lib/state/session.svelte';
	import { socket, socketStatus } from '$lib/socket/index.svelte';
	import { topics, type ServerMessage } from '$lib/socket/messages';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import * as Alert from '$lib/components/ui/alert';
	import * as Dialog from '$lib/components/ui/dialog';
	import Problem from '$lib/components/problem.svelte';
	import ConfigSetting from '$lib/components/config-setting.svelte';
	import ConfigRaw from '$lib/components/config-raw.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Search from '@lucide/svelte/icons/search';
	import History from '@lucide/svelte/icons/history';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');
	const file = $derived(page.params.file ?? '');

	let instance = $state<Instance | null>(null);
	let schema = $state<ConfigSchema | null>(null);
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let query = $state('');
	let reviewing = $state(false);

	/**
	 * What the form holds, and what the file held when it was read.
	 *
	 * Two maps rather than a list of pending changes: every control binds straight into
	 * `edits`, and a setting is changed exactly when the two disagree. Editing a value back
	 * to what it was therefore stops being a change, which is what makes the count in the
	 * save bar and the list in the dialog the same answer.
	 */
	let edits = $state<Record<string, ConfigValue>>({});
	let original = $state<Record<string, ConfigValue>>({});

	/**
	 * The versions the panel kept, and which one the form is comparing against.
	 *
	 * Both are absent until the panel has written this file once, which is the common case
	 * and not a failure — the comparison is simply not offered.
	 */
	let kept = $state<Record<ConfigCopyName, ConfigCopy | null>>({ original: null, previous: null });
	let compare = $state<ConfigCopyName | 'off'>('original');

	const reference = $derived(compare === 'off' ? null : kept[compare]);
	const refValues = $derived(reference ? valuesOf(reference) : {});

	/** Settings whose value in the reference differs from the file. Compared against the
	 * file rather than the pending edits, so the list answers "what has the panel changed"
	 * and does not shift while the operator types. Keys the current file no longer has are
	 * dropped: restoring one would patch a setting that does not exist. */
	const differences = $derived(
		Object.keys(refValues).filter(
			(field) => field in original && String(refValues[field]) !== String(original[field])
		)
	);

	const allowed = $derived(session.allowed(id));
	const canEdit = $derived(allowed.includes(actions.configEdit));
	const canRaw = $derived(allowed.includes(actions.configRaw));

	/**
	 * Which editor is on screen, and whether the raw one has ever been opened.
	 *
	 * Both stay mounted once shown, so switching back does not silently throw away what was
	 * typed in the other. The raw tab is unavailable while the form has pending changes:
	 * the two edit the same bytes, and a raw save would leave the form holding a schema
	 * parsed from a file that no longer exists.
	 */
	let tab = $state<'form' | 'raw'>('form');
	let rawOpened = $state(false);

	/** Why editing is unavailable, or null when it is available (B11). The daemon refuses a
	 * write on a running server independently; this exists so the refusal is legible before
	 * the click rather than after it. */
	const blocked = $derived.by(() => {
		if (!instance) return 'Loading this server.';
		if (instance.state === 'running') {
			return 'This server is running. Stop it to change its settings.';
		}
		if (instance.state !== 'stopped') {
			return `This server is ${instance.state.replaceAll('_', ' ')}. Settings change only on a stopped server.`;
		}
		return null;
	});
	const editable = $derived(canEdit && blocked === null && !saving);

	const changed = $derived(Object.keys(edits).filter((f) => edits[f] !== original[f]));

	const apiError = $derived(failure instanceof ApiError ? failure : null);
	function problem(field: string): string {
		return apiError?.field(field) ?? '';
	}

	const needle = $derived(query.trim().toLowerCase());
	/** The file as it will be shown: sections keep their order and their names, and a
	 * section with nothing matching drops out rather than standing empty. */
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

	// Subscribe, then fetch (G3, `14 §7.2`). The state topic is what tells this page the
	// server was started from another tab, which is the difference between a disabled form
	// and a 409 the operator has to read.
	$effect(() => {
		const off = socket.subscribe(topics.state(id), (m: ServerMessage) => {
			if (m.type !== 'state' || !instance) return;
			instance = { ...instance, state: m.state, restart_required: m.restart_required };
		});
		void load();
		return off;
	});

	let lastStatus = $state(socketStatus.value);
	$effect(() => {
		const status = socketStatus.value;
		if (status === 'open' && lastStatus !== 'open') void load();
		lastStatus = status;
	});

	async function load() {
		try {
			instance = await instances.get(id);
			take(await configs.read(id, file));
			await loadCopies();
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	/** A file the panel has never written has no copy to compare against, which is a 404 and
	 * the ordinary case. Nothing is reported: the comparison simply is not offered. */
	async function loadCopies() {
		const [asFound, beforeLastSave] = await Promise.all(
			copies.map((which) => configs.copy(id, file, which).catch(() => null))
		);
		kept = { original: asFound, previous: beforeLastSave };
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

	/** F4: the form does not predict the save. The write is confirmed in the dialog, and
	 * what the file holds afterwards is read back from the daemon rather than assumed. */
	async function saveConfirmed() {
		reviewing = false;
		saving = true;
		failure = null;
		try {
			const body: Record<string, ConfigValue> = {};
			for (const field of changed) body[field] = edits[field];
			await configs.patch(id, file, body);
			await load();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	/** Position, not name: a hand-edited file may write the same section header twice, and
	 * the daemon reports it as two sections in file order rather than merging them. */
	function anchor(index: number): string {
		return `section-${index}`;
	}

	const dateFormat = new Intl.DateTimeFormat(undefined, {
		dateStyle: 'medium',
		timeStyle: 'short'
	});
	function when(timestamp: string | undefined): string {
		const date = new Date(timestamp ?? '');
		return timestamp && !Number.isNaN(date.getTime()) ? dateFormat.format(date) : '';
	}

	/**
	 * The comparisons on offer. Labelled by what they are rather than by age: the file on
	 * screen is the newest version there is, so calling a kept copy "latest" would name the
	 * one thing it is not.
	 */
	const choices = $derived([
		...(kept.original ? [{ key: 'original' as const, label: 'the original' }] : []),
		...(kept.previous ? [{ key: 'previous' as const, label: 'before the last save' }] : []),
		{ key: 'off' as const, label: 'nothing' }
	]);

	/** Folds the reference's values into the pending edits. It writes nothing on its own —
	 * the operator confirms in the same dialog every other change goes through (F4). */
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

<div class="mx-auto grid max-w-5xl gap-6 p-6">
	<header class="grid gap-3">
		<Button
			variant="ghost"
			size="sm"
			class="justify-self-start"
			href={resolve('/instances/[id]/configs', { id })}
		>
			<ArrowLeft />
			Settings files
		</Button>
		<div class="grid gap-1">
			<h1 class="font-mono text-lg font-semibold">{file}</h1>
			<p class="text-sm text-muted-foreground">
				{schema?.plugin || 'No plugin named in this file'}
			</p>
		</div>
	</header>

	<!--
		Only where a copy exists — a file the panel has never written has nothing to compare
		against, and an empty control saying so would be noise on most files. The comparison
		is by setting, not by line: two of these values differing is the question a config
		screen is asked, and it survives a plugin rewriting the file around them.
	-->
	{#if choices.length > 1}
		<div class="grid gap-3 rounded-md border p-4">
			<div class="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
				<span class="text-muted-foreground">Compare with</span>
				{#each choices as choice (choice.key)}
					<button
						type="button"
						class="rounded-md px-2 py-1 {compare === choice.key
							? 'bg-secondary font-medium'
							: 'text-muted-foreground hover:text-foreground'}"
						aria-pressed={compare === choice.key}
						onclick={() => (compare = choice.key)}
					>
						{choice.label}
					</button>
				{/each}
			</div>

			{#if reference}
				{#if differences.length === 0}
					<p class="text-sm text-muted-foreground">
						Nothing differs from this version, kept {when(reference.captured_at)}.
					</p>
				{:else}
					<details class="text-sm">
						<summary class="cursor-pointer text-muted-foreground">
							{differences.length}
							{differences.length === 1 ? 'setting differs' : 'settings differ'} from this version, kept
							{when(reference.captured_at)}
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
							<!-- Puts the values back into the form, where the same confirmation every
							     other change goes through still applies. -->
							<Button variant="outline" size="sm" disabled={!editable} onclick={restoreAll}>
								<History />
								Restore {differences.length} to {choices.find((c) => c.key === compare)?.label}
							</Button>
						{/if}
						{#if canRaw}
							<!-- This list is by setting. A save that changed a comment or the file's
							     shape is only visible line by line, which is the raw view's job. -->
							<Button
								variant="ghost"
								size="sm"
								onclick={() => {
									rawOpened = true;
									tab = 'raw';
								}}
							>
								See it line by line
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

	{#if canEdit && blocked}
		<p class="text-sm text-muted-foreground" data-testid="config-actions-blocked">{blocked}</p>
	{:else if !canEdit && !loading}
		<p class="text-sm text-muted-foreground">You can read these settings but not change them.</p>
	{/if}

	{#if instance?.restart_required}
		<Alert.Root>
			<TriangleAlert />
			<Alert.Title>Restart required</Alert.Title>
			<Alert.Description>
				Settings changed since this server started. The running server is still using the old ones.
			</Alert.Description>
		</Alert.Root>
	{/if}

	{#if canRaw}
		<!-- Two buttons rather than a tabs component: there are two of them, and the panel
		     does not own one yet. -->
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
	{:else}
		<div class="grid gap-6" class:hidden={tab !== 'form'}>
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
				<!-- A real `.cfg` runs past a hundred settings, so the file's own grouping is the
			     way through it. The names are the section headers as written. -->
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

	{#if rawOpened}
		<div class:hidden={tab !== 'raw'}>
			<ConfigRaw
				{id}
				{file}
				{compare}
				editable={canRaw && blocked === null}
				onsaved={() => void load()}
			/>
		</div>
	{/if}
</div>

{#if tab === 'form' && changed.length > 0}
	<!-- The one thing this screen is for: an operator changed something in a file of a
	     hundred settings and needs to know what, before it is written. -->
	<div class="sticky bottom-0 border-t bg-background/95 backdrop-blur">
		<div class="mx-auto flex max-w-5xl flex-wrap items-center justify-between gap-3 p-4">
			<p class="text-sm">
				{changed.length}
				{changed.length === 1 ? 'setting' : 'settings'} changed
			</p>
			<div class="flex gap-2">
				<Button variant="ghost" size="sm" onclick={discard} disabled={saving}>Discard</Button>
				<Button size="sm" onclick={() => (reviewing = true)} disabled={!editable}>
					Review changes
				</Button>
			</div>
		</div>
	</div>
{/if}

<!--
	The diff, before anything is written. Nothing sends a patch except this dialog's
	confirm: a form of a hundred controls is exactly where a change gets made by accident,
	and the file on the other side is one an operator has often hand-edited.
-->
<Dialog.Root bind:open={reviewing}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title
				>Save {changed.length} {changed.length === 1 ? 'change' : 'changes'}?</Dialog.Title
			>
			<Dialog.Description>
				The rest of the file is untouched, and the version before this save is kept beside it.
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
