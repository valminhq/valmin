<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import {
		actions,
		instances,
		isTransient,
		STATUS_TEXT_MAX,
		type GameOptions,
		type Instance,
		type PatchInstance
	} from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import { tick } from 'svelte';
	import Field, { focusFirstInvalid } from '$lib/components/field.svelte';
	import Problem from '$lib/components/problem.svelte';
	import RestartNotice from '$lib/components/restart-notice.svelte';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import { manifest, type TemplateCode } from '$lib/api/manifest';
	import CopyButton from '$lib/components/copy-button.svelte';
	import { unsaved } from '$lib/state/dirty.svelte';
	import Copy from '@lucide/svelte/icons/copy';
	import Download from '@lucide/svelte/icons/download';
	import Lock from '@lucide/svelte/icons/lock';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');
	// The whole link, so an operator can copy it out of the panel and send it to a friend.
	const statusURL = $derived(`${page.url.origin}/status/${id}`);
	const textareaClass =
		'w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-destructive/20';

	let instance = $state<Instance | null>(null);
	let options = $state<GameOptions | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);
	let saving = $state(false);
	let confirming = $state(false);
	let exporting = $state(false);
	let template = $state<TemplateCode | null>(null);
	let coding = $state(false);
	let deleteOpen = $state(false);

	let panelName = $state('');
	let serverName = $state('');
	let password = $state('');
	let isPublic = $state(false);
	let statusPublished = $state(false);
	let statusNotice = $state('');
	let statusConnectInfo = $state('');
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
	const canClone = $derived(allowed.includes(actions.clone));
	const canDelete = $derived(allowed.includes(actions.remove));
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

	async function createCode() {
		coding = true;
		failure = null;
		try {
			template = await manifest.code(id);
		} catch (err) {
			failure = err;
		} finally {
			coding = false;
		}
	}

	/** One line on what the code holds and leaves out. */
	function codeSummary(t: TemplateCode): string {
		const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`;
		const parts = [
			`${t.code.length} characters`,
			plural(t.mods, 'mod'),
			plural(t.settings, 'changed setting')
		];
		if (t.secrets_left_out > 0)
			parts.push(`${plural(t.secrets_left_out, 'secret-looking setting')} left out`);
		if (t.disabled_left_out > 0)
			parts.push(`${plural(t.disabled_left_out, 'disabled mod')} left out`);
		return parts.join(' · ');
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
		panelName = row.name;
		serverName = row.server_name;
		password = '';
		isPublic = row.public;
		statusPublished = row.status_published;
		statusNotice = row.status_notice ?? '';
		statusConnectInfo = row.status_connect_info ?? '';
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
		if (panelName.trim() !== instance.name) fields.push('name');
		if (serverName.trim() !== instance.server_name) fields.push('server_name');
		if (password !== '') fields.push('password');
		if (isPublic !== instance.public) fields.push('public');
		if (statusPublished !== instance.status_published) fields.push('status_published');
		if (statusNotice.trim() !== (instance.status_notice ?? '')) fields.push('status_notice');
		if (statusConnectInfo.trim() !== (instance.status_connect_info ?? '')) {
			fields.push('status_connect_info');
		}
		if (crossplay !== instance.crossplay) fields.push('crossplay');
		if (preset !== (instance.preset ?? '')) fields.push('preset');
		if (normalise(modifiers) !== normalise(decodeModifiers(instance.modifiers))) {
			fields.push('modifiers');
		}
		if (memLimitMB !== instance.mem_limit_mb) fields.push('mem_limit_mb');
		if ((cpuLimit ?? null) !== instance.cpu_limit) fields.push('cpu_limit');
		return fields;
	});

	unsaved(() => changed.length > 0);

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
		if (panelName.trim() === '') problems.name = 'Give this server a name.';
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
		if (changed.includes('name')) body.name = panelName.trim();
		if (changed.includes('server_name')) body.server_name = serverName.trim();
		if (changed.includes('password')) body.password = password;
		if (changed.includes('public')) body.public = isPublic;
		if (changed.includes('status_published')) body.status_published = statusPublished;
		if (changed.includes('status_notice')) body.status_notice = statusNotice.trim();
		if (changed.includes('status_connect_info')) {
			body.status_connect_info = statusConnectInfo.trim();
		}
		if (changed.includes('crossplay')) body.crossplay = crossplay;
		if (changed.includes('preset')) body.preset = preset;
		if (changed.includes('modifiers')) body.modifiers = setModifiers;
		if (changed.includes('mem_limit_mb') && memLimitMB !== undefined) {
			body.mem_limit_mb = memLimitMB;
		}
		if (changed.includes('cpu_limit')) body.cpu_limit = cpuLimit ?? null;
		try {
			adopt(await instances.patch(id, body));
			// The header and the server switcher read the name from the list.
			if (body.name !== undefined) void instanceList.load();
		} catch (err) {
			failure = err;
			// After the rejected fields have rendered, not before.
			void tick().then(() => focusFirstInvalid());
		} finally {
			saving = false;
		}
	}

	/** Deletes the server, keeping its worlds on disk, and returns to the server list. */
	async function remove() {
		failure = null;
		try {
			await instances.remove(id, true);
			await goto(resolve('/'));
		} catch (err) {
			failure = err;
		}
	}
</script>

<div class="grid gap-6">
	<header class="grid gap-3">
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="grid gap-1">
				<h2 class="text-2xl font-semibold tracking-tight">Server settings</h2>
				<p class="text-sm text-muted-foreground">
					Names, connections, the status page, gameplay and resource limits.
				</p>
			</div>
		</div>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !instance}
		<p class="text-sm text-muted-foreground">This server is not here.</p>
	{:else}
		<RestartNotice {instance} />

		{#if !canEdit && !canEditLimits}
			<p class="text-sm text-muted-foreground" data-testid="settings-blocked">
				You can see these settings but not change them.
			</p>
		{/if}

		<div class="grid gap-6">
			<div class="grid items-start gap-6 lg:grid-cols-2">
				<Card.Root>
					<Card.Header>
						<Card.Title>Server identity</Card.Title>
						<Card.Description
							>The names this server goes by and the password players use to join.</Card.Description
						>
					</Card.Header>
					<Card.Content class="grid gap-4">
						<Field
							id="name"
							label="Panel name"
							hint="What this server is called in the panel."
							error={problem('name')}
						>
							{#snippet children(field)}
								<Input id="name" bind:value={panelName} disabled={!canEdit} {...field} />
							{/snippet}
						</Field>

						<Field
							id="server_name"
							label="Server name"
							hint="What players see in the server browser."
							error={problem('server_name')}
						>
							{#snippet children(field)}
								<Input id="server_name" bind:value={serverName} disabled={!canEdit} {...field} />
							{/snippet}
						</Field>

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
							<p class="text-sm text-muted-foreground">
								The name of the save file on disk. It cannot be changed here.
							</p>
						</div>

						<Field
							id="password"
							label="Server password"
							hint="Leave blank to keep the current password. The overview can show the current one."
							error={problem('password')}
						>
							{#snippet children(field)}
								<Input
									id="password"
									type="password"
									autocomplete="new-password"
									placeholder="Unchanged"
									disabled={!canEdit}
									bind:value={password}
									{...field}
								/>
							{/snippet}
						</Field>
					</Card.Content>
				</Card.Root>

				<Card.Root>
					<Card.Header>
						<Card.Title>Connections</Card.Title>
						<Card.Description>How players find and join this server.</Card.Description>
					</Card.Header>
					<Card.Content class="grid gap-4">
						<div class="flex items-center justify-between gap-4">
							<div class="grid gap-1">
								<Label for="public">List publicly</Label>
								<p class="text-sm text-muted-foreground">
									Show this server in the community browser.
								</p>
							</div>
							<Switch id="public" disabled={!canEdit} bind:checked={isPublic} />
						</div>

						<div class="flex items-center justify-between gap-4">
							<div class="grid gap-1">
								<Label for="crossplay">Crossplay</Label>
								<p class="text-sm text-muted-foreground">
									Lets players on other platforms find and join this server.
								</p>
							</div>
							<Switch id="crossplay" disabled={!canEdit} bind:checked={crossplay} />
						</div>

						<!--
					`03 §1.4` rule 5, with the list from the daemon so the panel cannot quietly stop
					warning (Q6). The join code is rendered above, not here.
				-->
						{#if crossplay && options && options.crossplay_untested.length > 0}
							<div
								class="grid gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3"
							>
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
						<Card.Title>Public status page</Card.Title>
						<Card.Description>
							A page anyone with the link can open without signing in. It shows this server's name,
							whether it is up or under maintenance, how many players are on it, and the text below.
						</Card.Description>
					</Card.Header>
					<Card.Content class="grid gap-4">
						<div class="flex items-center justify-between gap-4">
							<div class="grid gap-1">
								<Label for="status_published">Publish the status page</Label>
								{#if instance.status_published}
									<a
										class="text-xs break-all underline underline-offset-4"
										href={resolve('/status/[id]', { id })}
										target="_blank"
										rel="noreferrer">{statusURL}</a
									>
								{:else}
									<p class="text-sm text-muted-foreground">Off unless you turn it on.</p>
								{/if}
							</div>
							<Switch id="status_published" disabled={!canEdit} bind:checked={statusPublished} />
						</div>

						{#if statusPublished}
							<Field
								id="status_notice"
								label="Status page notice"
								hint={`Shown on the status page, for example "Down for the update until 20:00". Plain text, up to ${STATUS_TEXT_MAX} characters.`}
								error={problem('status_notice')}
							>
								{#snippet children(field)}
									<textarea
										id="status_notice"
										rows="2"
										maxlength={STATUS_TEXT_MAX}
										disabled={!canEdit}
										class={textareaClass}
										bind:value={statusNotice}
										{...field}></textarea>
								{/snippet}
							</Field>

							<Field
								id="status_connect_info"
								label="How to join"
								hint={`Shown on the status page: the address, where to get the password, and any mods players need. Plain text, up to ${STATUS_TEXT_MAX} characters.`}
								error={problem('status_connect_info')}
							>
								{#snippet children(field)}
									<textarea
										id="status_connect_info"
										rows="3"
										maxlength={STATUS_TEXT_MAX}
										disabled={!canEdit}
										class={textareaClass}
										bind:value={statusConnectInfo}
										{...field}></textarea>
								{/snippet}
							</Field>
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
								<p class="text-sm text-muted-foreground">The game may accept other presets too.</p>
							{/if}
							{#if problem('preset')}<p class="text-sm text-destructive">
									{problem('preset')}
								</p>{/if}
						</div>

						{#if options}
							{@const count = Object.keys(setModifiers).length}
							<details class="rounded-lg border">
								<summary
									class="cursor-pointer rounded-lg p-3 text-sm font-medium focus-visible:outline-2 focus-visible:outline-ring"
									>World modifiers <span class="ml-2 font-normal text-muted-foreground"
										>{count ? `${count} set` : 'Optional'}</span
									></summary
								>
								<div class="grid gap-4 border-t p-3">
									{#if !options.modifier_values_measured}
										<!--
									The five axes are measured (`03 §1.3`); their legal values are not, since the
									`.fwl`'s stored form is not proven to be the command-line grammar (E8).
								-->
										<p class="text-sm text-muted-foreground">
											The game supports these modifiers, but their accepted values have not been
											verified. Leave a field blank unless you know which value to use.
										</p>
									{/if}
									{#each options.modifier_keys as key (key)}
										<div class="grid gap-2">
											<Label for={`modifier-${key}`}>{key}</Label>
											<Input
												id={`modifier-${key}`}
												value={modifiers[key] ?? ''}
												placeholder="Game default"
												disabled={!canEdit}
												oninput={(event) => (modifiers[key] = event.currentTarget.value)}
											/>
										</div>
									{/each}
								</div>
							</details>
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
							The panel uses these limits when it builds the container on the next start. Saving
							does not change the running container.
						</Card.Description>
					</Card.Header>
					<Card.Content class="grid gap-4 sm:grid-cols-2" data-testid="limit-controls">
						<Field
							id="mem_limit_mb"
							label="Memory limit (MB)"
							hint={minMemory !== undefined
								? `At least ${minMemory} MB. Use generous headroom: exhausting the limit can interrupt a save.`
								: 'Use generous headroom: exhausting the limit can interrupt a save.'}
							error={problem('mem_limit_mb')}
						>
							{#snippet children(field)}
								<Input
									id="mem_limit_mb"
									type="number"
									min={minMemory}
									step="256"
									disabled={!canEditLimits}
									bind:value={memLimitMB}
									{...field}
								/>
							{/snippet}
						</Field>

						<Field
							id="cpu_limit"
							label="CPU limit (cores)"
							hint="Leave blank for no CPU quota. A tight quota can hurt a simulation that depends heavily on one core."
							error={problem('cpu_limit')}
						>
							{#snippet children(field)}
								<Input
									id="cpu_limit"
									type="number"
									min="0.01"
									step="0.25"
									placeholder="No quota"
									disabled={!canEditLimits}
									bind:value={cpuLimit}
									{...field}
								/>
							{/snippet}
						</Field>

						{#if !canEditLimits}
							<p class="text-xs text-muted-foreground sm:col-span-2">
								You can see these limits but not change them.
							</p>
						{/if}
					</Card.Content>
				</Card.Root>
			</div>

			{#if canEdit || canEditLimits}
				<div
					class="sticky bottom-0 -mx-4 flex flex-wrap items-center justify-between gap-3 border-t bg-background/95 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6"
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
		</div>

		{#if canClone || canExportManifest || canDelete}
			<Card.Root>
				<Card.Header>
					<Card.Title>Manage server</Card.Title>
					<Card.Description
						>These take effect at once. Save changes does not apply to them.</Card.Description
					>
				</Card.Header>
				<Card.Content class="grid divide-y">
					{#if canClone}
						<div class="flex flex-wrap items-center justify-between gap-3 pb-4">
							<div class="grid gap-1">
								<p class="text-sm font-medium">Clone</p>
								<p class="text-sm text-muted-foreground">
									{instance.state === 'stopped'
										? 'Make a new server with a copy of this one: its world, mods and settings.'
										: 'Stop this server to clone it.'}
								</p>
							</div>
							<Button
								variant="outline"
								size="sm"
								disabled={instance.state !== 'stopped'}
								href={resolve('/instances/[id]/clone', { id })}
							>
								<Copy />
								Clone
							</Button>
						</div>
					{/if}

					{#if canExportManifest}
						<div class="grid gap-4 py-4 first:pt-0">
							<div class="flex flex-wrap items-center justify-between gap-3">
								<div class="grid gap-1">
									<p class="text-sm font-medium">Share as a template</p>
									<p class="text-sm text-muted-foreground">
										The template file holds the launch settings, the mods at their installed
										versions and every config file. It carries no password and no world, but a mod's
										config can hold a key or a webhook — read it before you share it.
									</p>
								</div>
								<Button variant="outline" size="sm" disabled={exporting} onclick={downloadManifest}>
									<Download />
									{exporting ? 'Preparing…' : 'Download template file'}
								</Button>
							</div>
							<div class="grid gap-2">
								<p class="text-sm text-muted-foreground">
									A template code is a short text to paste under New from template on another panel.
									It holds the enabled mods and the settings changed in this panel.
								</p>
								<Button
									variant="outline"
									size="sm"
									class="justify-self-start"
									disabled={coding}
									onclick={createCode}
								>
									{coding
										? 'Preparing…'
										: template
											? 'Refresh template code'
											: 'Create template code'}
								</Button>
								{#if template}
									<textarea
										readonly
										rows="4"
										aria-label="Template code"
										class="{textareaClass} font-mono text-xs break-all"
										onfocus={(e) => e.currentTarget.select()}>{template.code}</textarea
									>
									<div class="flex flex-wrap items-center gap-3">
										<CopyButton value={template.code} label="Copy code" size="sm" />
										<span class="text-sm text-muted-foreground">{codeSummary(template)}</span>
									</div>
								{/if}
							</div>
						</div>
					{/if}

					{#if canDelete}
						<div class="flex flex-wrap items-center justify-between gap-3 pt-4 first:pt-0">
							<div class="grid gap-1">
								<p class="text-sm font-medium">Delete this server</p>
								<p class="text-sm text-muted-foreground">
									Removes the container and this server's settings. Its worlds stay on disk.
								</p>
							</div>
							<Button
								variant="destructive"
								size="sm"
								disabled={isTransient(instance.state)}
								onclick={() => (deleteOpen = true)}
							>
								<Trash2 />
								Delete server
							</Button>
						</div>
					{/if}
				</Card.Content>
			</Card.Root>

			<DestructiveConfirm
				bind:open={deleteOpen}
				name={instance.name}
				title="Delete {instance.name}?"
				confirmLabel="Delete server"
				description="The container and this server's settings are removed. Its worlds are kept on disk — nothing here deletes a world."
				onconfirm={remove}
			/>
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
