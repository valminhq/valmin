<script lang="ts">
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance, type Reachability } from '#lib/api/instances.js';
	import { mods, type ExportCode, type ExportPreview } from '#lib/api/mods.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import CopyButton from '#lib/components/copy-button.svelte';
	import Problem from '#lib/components/problem.svelte';
	import Eye from '@lucide/svelte/icons/eye';
	import EyeOff from '@lucide/svelte/icons/eye-off';
	import Gamepad2 from '@lucide/svelte/icons/gamepad-2';
	import CircleCheck from '@lucide/svelte/icons/circle-check';
	import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { successText, warningText } from '#lib/tone.js';

	let { instance }: { instance: Instance } = $props();

	const canView = $derived(session.can(instance.id, actions.view));
	const canMods = $derived(session.can(instance.id, actions.modsList));
	const canSettings = $derived(session.can(instance.id, actions.settings));

	let password = $state<string | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(false);

	let report = $state<Reachability | null>(null);
	let checkFailure = $state<unknown>(null);
	let checking = $state(false);

	let preview = $state<ExportPreview | null>(null);
	let profileCode = $state<ExportCode | null>(null);
	let codeFailure = $state<unknown>(null);
	let creating = $state(false);

	// Identifies the latest check: a reply from an earlier one, after the card moved to another
	// server or the server stopped, is dropped.
	let checkRun = 0;

	const id = $derived(instance.id);
	const canCheck = $derived(canView && instance.state === 'running');
	const modded = $derived(instance.modded);
	const clientMods = $derived(preview?.mods.length ?? 0);

	// Forget a revealed password, a check and a profile code when the card moves to another server.
	$effect(() => {
		void id;
		password = null;
		failure = null;
		loading = false;
		report = null;
		checkFailure = null;
		checking = false;
		checkRun++;
		profileCode = null;
		codeFailure = null;
		creating = false;
	});

	// A verdict and its error describe the run they were taken in, so they go when the server stops.
	$effect(() => {
		if (instance.state !== 'running') {
			report = null;
			checkFailure = null;
			checking = false;
			checkRun++;
		}
	});

	// Reads which mods players need, for a modded server whose mods the user may see. It is a
	// local read; the profile code itself is uploaded only on request.
	$effect(() => {
		const target = id;
		preview = null;
		if (!canMods || !modded) return;
		let current = true;
		mods.exportPreview(target).then(
			(p) => {
				if (current) preview = p;
			},
			() => {}
		);
		return () => {
			current = false;
		};
	});

	/** Reads the password on request only: every read is audited. */
	async function reveal() {
		const target = id;
		loading = true;
		failure = null;
		try {
			const value = await instances.password(target);
			if (target === id) password = value;
		} catch (err) {
			if (target === id) failure = err;
		} finally {
			if (target === id) loading = false;
		}
	}

	/** Measures reachability on request only: each check makes outbound requests. */
	async function check() {
		const run = ++checkRun;
		checking = true;
		checkFailure = null;
		try {
			const result = await instances.reachability(id);
			if (run === checkRun) report = result;
		} catch (err) {
			if (run === checkRun) checkFailure = err;
		} finally {
			if (run === checkRun) checking = false;
		}
	}

	/** Uploads the player modpack on request only: each upload publishes the mod list. */
	async function createCode() {
		const target = id;
		creating = true;
		codeFailure = null;
		try {
			const code = await mods.exportCode(target);
			if (target === id) profileCode = code;
		} catch (err) {
			if (target === id) codeFailure = err;
		} finally {
			if (target === id) creating = false;
		}
	}
</script>

<section class="grid gap-4 rounded-lg border bg-card p-4" aria-labelledby="connect-heading">
	<h2 id="connect-heading" class="text-sm font-medium">Join this server</h2>

	<ol class="grid list-decimal gap-4 pl-5 text-sm marker:text-muted-foreground">
		{#if clientMods > 0}
			<li class="grid gap-2">
				<span class="font-medium">Install the mods</span>
				<p class="text-muted-foreground">
					Players need {clientMods === 1 ? '1 mod' : `${clientMods} mods`} that match this server. Install
					<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
					<a
						class="underline underline-offset-4"
						href="https://thunderstore.io/c/valheim/p/Kesomannen/GaleModManager/"
						target="_blank"
						rel="noreferrer">Gale</a
					>
					or
					<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
					<a
						class="underline underline-offset-4"
						href="https://thunderstore.io/c/valheim/p/ebkr/r2modman/"
						target="_blank"
						rel="noreferrer">r2modman</a
					>, create a Valheim profile, and import this profile code into it.
				</p>
				{#if preview && preview.conflicts.length > 0}
					<p class="text-muted-foreground">
						The modpack has a conflict to fix before it can be shared. See
						<a
							class="underline underline-offset-4"
							href={resolve('/instances/[id]/mods?tab=players', { id })}>Player modpack</a
						>.
					</p>
				{:else if profileCode}
					<div class="flex flex-wrap items-center gap-2">
						<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm break-all select-all"
							>{profileCode.code}</code
						>
						<CopyButton
							value={profileCode.code}
							ariaLabel="Copy profile code"
							variant="ghost"
							size="sm"
						/>
						{#if profileCode.registry === 'hexium'}
							<span class="text-xs text-muted-foreground">Works in Gale only.</span>
						{/if}
					</div>
				{:else}
					<div>
						<Button variant="outline" size="sm" disabled={creating} onclick={createCode}>
							{creating ? 'Creating…' : 'Create profile code'}
						</Button>
					</div>
				{/if}
				<Problem error={codeFailure} />
			</li>
		{/if}

		<li class="grid gap-2">
			<span class="font-medium">Connect</span>
			{#if instance.crossplay}
				<div class="flex flex-wrap items-center gap-2">
					<span class="text-muted-foreground">Join code</span>
					{#if instance.crossplay_join_code}
						<code
							class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm font-semibold tracking-wider select-all"
							>{instance.crossplay_join_code}</code
						>
						<CopyButton
							value={instance.crossplay_join_code}
							label="Copy"
							variant="ghost"
							size="sm"
						/>
					{:else}
						<!-- The code is logged seconds after the server reaches running, and a restart
						     replaces it, so its absence has two causes. -->
						<span class="text-muted-foreground">
							{instance.state === 'running'
								? 'Not logged yet — it appears within a few seconds of the server starting.'
								: 'Available once the server is running.'}
						</span>
					{/if}
				</div>
			{:else if instance.join_address}
				<div class="flex flex-wrap items-center gap-2">
					<span class="text-muted-foreground">Address</span>
					<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm break-all select-all"
						>{instance.join_address}</code
					>
					<CopyButton
						value={instance.join_address}
						ariaLabel="Copy join address"
						variant="ghost"
						size="sm"
					/>
					{#if instance.steam_join_url}
						<Button variant="outline" size="sm" href={instance.steam_join_url}>
							<Gamepad2 />
							Join with Steam
						</Button>
					{/if}
				</div>
				{#if instance.steam_join_url}
					<p class="text-xs text-muted-foreground">
						Close the game before using Join with Steam: it starts the game and connects, and does
						nothing while the game is already running.
					</p>
				{/if}
			{:else}
				<p class="text-muted-foreground">
					<span class="tabular-nums">UDP {instance.base_port}–{instance.base_port + 1}</span>
					on this host's address. No join address is set for this server{#if canSettings}; add one
						in
						<a
							class="underline underline-offset-4"
							href={resolve('/instances/[id]/settings', { id })}>Settings</a
						>{/if}.
				</p>
			{/if}
		</li>

		{#if canView}
			<li class="grid gap-2">
				<span class="font-medium">Enter the password</span>
				<div class="flex flex-wrap items-center gap-2">
					{#if password !== null}
						<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm select-all"
							>{password}</code
						>
						<CopyButton value={password} ariaLabel="Copy password" variant="ghost" size="sm" />
						<Button variant="ghost" size="sm" onclick={() => (password = null)}>
							<EyeOff />
							Hide
						</Button>
					{:else}
						<Button variant="outline" size="sm" disabled={loading} onclick={reveal}>
							<Eye />
							Show password
						</Button>
					{/if}
				</div>
				{#if instance.restart_required}
					<p class="text-xs text-muted-foreground">
						A restart is pending. If the password was changed, the running server keeps the previous
						password until it restarts.
					</p>
				{/if}
				<Problem error={failure} />
			</li>
		{/if}
	</ol>

	<p class="flex flex-wrap gap-x-4 gap-y-1 text-sm text-muted-foreground">
		<span>World <span class="text-foreground">{instance.world_name}</span></span>
		{#if instance.public}
			<span
				>Listed in the community browser as <span class="text-foreground"
					>{instance.server_name}</span
				></span
			>
		{/if}
	</p>

	{#if canCheck}
		<div class="grid gap-2">
			<div>
				<Button variant="outline" size="sm" disabled={checking} onclick={check}>
					{checking ? 'Checking…' : 'Check reachability'}
				</Button>
			</div>
			<Problem error={checkFailure} />
			{#if report}
				<div class="grid gap-2 rounded-md border p-3 text-sm" role="status">
					<p class="flex items-start gap-2 font-medium">
						{#if report.status === 'ok'}
							<CircleCheck class={['mt-0.5 size-4 shrink-0', successText]} />
						{:else if report.status === 'warn'}
							<TriangleAlert class={['mt-0.5 size-4 shrink-0', warningText]} />
						{:else}
							<CircleQuestionMark class="mt-0.5 size-4 shrink-0 text-muted-foreground" />
						{/if}
						<span>{report.detail}</span>
					</p>
					{#if report.public_address}
						<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
							<span class="text-muted-foreground">Public address</span>
							<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm select-all"
								>{report.public_address}</code
							>
							<CopyButton
								value={report.public_address}
								ariaLabel="Copy public address"
								variant="ghost"
								size="sm"
							/>
						</div>
					{/if}
					{#if report.remedy}
						<p class="text-muted-foreground">{report.remedy}</p>
					{/if}
					{#if report.steps.length > 0}
						<ul class="grid gap-1 text-xs text-muted-foreground">
							{#each report.steps as step (step.name)}
								<li>{step.detail}</li>
							{/each}
						</ul>
					{/if}
				</div>
			{/if}
		</div>
	{/if}
</section>
