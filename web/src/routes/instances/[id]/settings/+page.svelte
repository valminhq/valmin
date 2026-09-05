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
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import Lock from '@lucide/svelte/icons/lock';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	const id = $derived(page.params.id ?? '');

	let instance = $state<Instance | null>(null);
	let options = $state<GameOptions | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(true);
	let saving = $state(false);
	let confirming = $state(false);

	let serverName = $state('');
	let password = $state('');
	let isPublic = $state(false);
	let crossplay = $state(false);
	let preset = $state('');
	let modifiers = $state<Record<string, string>>({});

	const allowed = $derived(session.allowed(id));
	const canEdit = $derived(allowed.includes(actions.settings));
	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const minPassword = $derived(options?.min_password_length ?? 5);

	$effect(() => {
		void load();
	});

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

	/** Take the daemon's row as the form's new baseline. Every write goes through here, so
	 * the screen never shows a value it merely sent (F4). */
	function adopt(row: Instance) {
		instance = row;
		serverName = row.server_name;
		password = '';
		isPublic = row.public;
		crossplay = row.crossplay;
		preset = row.preset ?? '';
		modifiers = decodeModifiers(row.modifiers);
	}

	/** `04 §2` stores the set as a JSON object in one column. A row written before the column
	 * existed, or by hand, is treated as no modifiers rather than as a broken screen. */
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
		if (crossplay !== instance.crossplay) fields.push('crossplay');
		if (preset !== (instance.preset ?? '')) fields.push('preset');
		if (normalise(modifiers) !== normalise(decodeModifiers(instance.modifiers))) {
			fields.push('modifiers');
		}
		return fields;
	});

	// `03 §1.3`'s three rules, client-side as a courtesy only — the daemon checks them against
	// the merged row and `08 §5.1` checks them again at container creation (G2). Rule 2 cannot
	// be fully checked here: an unchanged password is not on this page, so renaming the server
	// into it is caught by the daemon and rendered on the field.
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
		return problems;
	});

	function problem(field: string): string | undefined {
		return localProblems[field] ?? apiError?.field(field);
	}

	const ready = $derived(
		canEdit && changed.length > 0 && Object.keys(localProblems).length === 0 && !saving
	);

	/** A new password locks every player out of the server until they are told it, so it is
	 * the one field here that asks twice (F5). The rest are undone by editing them back. */
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
		if (changed.includes('crossplay')) body.crossplay = crossplay;
		if (changed.includes('preset')) body.preset = preset;
		if (changed.includes('modifiers')) body.modifiers = setModifiers;
		try {
			adopt(await instances.patch(id, body));
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
</script>

<div class="mx-auto grid max-w-2xl gap-6 p-6">
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
				<h1 class="text-lg font-semibold">Server settings</h1>
				<p class="text-sm text-muted-foreground">
					How this server introduces itself and who can reach it. Changes are saved now and take
					effect on the next start.
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

		{#if !canEdit}
			<p class="text-sm text-muted-foreground" data-testid="settings-blocked">
				You can see these settings but not change them.
			</p>
		{/if}

		<Card.Root>
			<Card.Header>
				<Card.Title>What players see</Card.Title>
				<Card.Description>The name in the browser and the password to get past it.</Card.Description
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
					Q48. Shown rather than omitted: `-world` names the save file basename
					(`03 §1.3`, ADR-077), so renaming it moves the `.db`/`.fwl` pair and needs
					`03 §4.1`'s handling — a job, not a column write. An operator who cannot find
					a setting concludes the panel is broken; one who is told concludes it is honest.
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
						This is the name of the save file on disk, so changing it moves the world rather than a
						setting. The panel cannot do that yet.
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
				<Card.Title>Who can find it</Card.Title>
				<Card.Description>
					Discovery only. Neither of these changes how the world is saved.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<div class="flex items-center justify-between gap-4">
					<div class="grid gap-1">
						<Label for="public">List publicly</Label>
						<p class="text-xs text-muted-foreground">Show this server in the community browser.</p>
					</div>
					<Switch id="public" disabled={!canEdit} bind:checked={isPublic} />
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
					saying it. Q6 blocks advertising crossplay as fully supported; the join code
					itself (Q25) is a separate, closed question and rendered above, not here.
				-->
				{#if crossplay && options}
					<div class="grid gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3">
						<p class="flex items-center gap-2 text-sm font-medium">
							<TriangleAlert class="size-4" />
							Untested combinations
						</p>
						<p class="text-sm text-muted-foreground">
							These have never been run, so the panel cannot say whether they work:
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
				<Card.Title>How the world plays</Card.Title>
				<Card.Description>
					Combat, death penalty, raids and the rest. These are the two settings on this page that
					reach past discovery and into the game itself.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<!--
					Q49 (E8). What a changed preset does to a world that already exists is unmeasured:
					`03 §1.3.1` measured which names the parser accepts, which is a different question.
					The screen ships both fields and claims nothing in either direction.
				-->
				<p
					class="flex items-start gap-2 rounded-lg border border-dashed border-muted-foreground/30 p-3 text-sm text-muted-foreground"
				>
					<TriangleAlert class="mt-0.5 size-4 shrink-0" />
					<span>
						Nobody has measured what these do to a world that already exists. They are known to
						shape a new one. Change them on an established world only if you are willing to find
						out.
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
							`03 §1.3.1`: the list was enumerated by feeding candidates to the real parser,
							which confirms what it is given and cannot enumerate what nobody tried.
						-->
						<p class="text-xs text-muted-foreground">
							Measured against build {options.build} by trying each value against the game itself. Other
							presets may exist; the panel does not refuse one it has not seen.
						</p>
					{/if}
					{#if problem('preset')}<p class="text-sm text-destructive">{problem('preset')}</p>{/if}
				</div>

				{#if options}
					{#if !options.modifier_values_measured}
						<!--
							E8. The five axes are measured (`03 §1.3`); their legal values are not. The
							only evidence is a `.fwl`'s stored `combat_default:…` form, which `03 §4.2`
							says in the same breath is not proven to be the command-line grammar.
						-->
						<p class="text-xs text-muted-foreground">
							The five axes below are measured; their accepted values are not. Leave one blank
							unless you know the value you want.
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

		<!-- Its own capability and its own confirmation: this one replaces world data, and the
		     save bar below has nothing to do with it. -->
		<WorldImport {instance} />

		{#if canEdit}
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
						Discard
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
				Change it
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
