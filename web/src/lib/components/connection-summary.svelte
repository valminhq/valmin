<script lang="ts">
	import { actions, instances, type Instance, type Reachability } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import CopyButton from '$lib/components/copy-button.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Eye from '@lucide/svelte/icons/eye';
	import EyeOff from '@lucide/svelte/icons/eye-off';
	import CircleCheck from '@lucide/svelte/icons/circle-check';
	import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let { instance }: { instance: Instance } = $props();

	const canView = $derived(session.can(instance.id, actions.view));

	let password = $state<string | null>(null);
	let failure = $state<unknown>(null);
	let loading = $state(false);

	let report = $state<Reachability | null>(null);
	let checkFailure = $state<unknown>(null);
	let checking = $state(false);

	// Identifies the latest check: a reply from an earlier one, after the card moved to another
	// server or the server stopped, is dropped.
	let checkRun = 0;

	const id = $derived(instance.id);
	const canCheck = $derived(canView && instance.state === 'running');

	// Forget a revealed password and a check when the card moves to another server.
	$effect(() => {
		void id;
		password = null;
		failure = null;
		report = null;
		checkFailure = null;
		checking = false;
		checkRun++;
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
			loading = false;
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
</script>

<section class="grid gap-3 rounded-lg border bg-card p-4" aria-labelledby="connect-heading">
	<h2 id="connect-heading" class="text-sm font-medium">Connect to this server</h2>

	<dl class="grid gap-2 text-sm">
		{#if instance.crossplay}
			<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
				<dt class="text-muted-foreground">Join code</dt>
				<dd class="flex items-center gap-2">
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
						<!-- The code is latched out of the log seconds after the server reaches running, and
						     a restart invalidates the previous one (Q25), so its absence has two causes and
						     the operator is told which applies. -->
						<span class="text-muted-foreground">
							{instance.state === 'running'
								? 'Not logged yet — it appears within a few seconds of the server starting.'
								: 'Available once the server is running.'}
						</span>
					{/if}
				</dd>
			</div>
		{/if}

		<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
			<dt class="text-muted-foreground">Port</dt>
			<dd class="tabular-nums">UDP {instance.base_port}–{instance.base_port + 1}</dd>
		</div>

		{#if instance.public}
			<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
				<dt class="text-muted-foreground">Community browser</dt>
				<dd>Listed as <span class="font-medium">{instance.server_name}</span></dd>
			</div>
		{/if}

		<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
			<dt class="text-muted-foreground">World</dt>
			<dd>{instance.world_name}</dd>
		</div>

		{#if canView}
			<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
				<dt class="text-muted-foreground">Password</dt>
				<dd class="flex items-center gap-2">
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
						<Button variant="ghost" size="sm" disabled={loading} onclick={reveal}>
							<Eye />
							Show
						</Button>
					{/if}
				</dd>
				{#if instance.restart_required}
					<dd class="basis-full text-xs text-muted-foreground">
						A restart is pending. If the password was changed, the running server keeps the previous
						password until it restarts.
					</dd>
				{/if}
			</div>
		{/if}
	</dl>
	<Problem error={failure} />

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
							<CircleCheck class="mt-0.5 size-4 shrink-0 text-green-600 dark:text-green-400" />
						{:else if report.status === 'warn'}
							<TriangleAlert class="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
						{:else}
							<CircleQuestionMark class="mt-0.5 size-4 shrink-0 text-muted-foreground" />
						{/if}
						<span>{report.detail}</span>
					</p>
					{#if report.public_address}
						<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
							<span class="text-muted-foreground">Join address</span>
							<code class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm select-all"
								>{report.public_address}</code
							>
							<CopyButton
								value={report.public_address}
								ariaLabel="Copy join address"
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

	<!-- `02 §5`: a container's address is not a route to it. Valmin knows the port it published on
	     the host and nothing about how a player reaches that host, so it says so rather than
	     offering an address that may not resolve. -->
	<p class="text-sm text-muted-foreground">
		Players connect to the address of the host this server runs on, with the port above. Valmin does
		not know that address on its own, and a check can only try it from this host.
	</p>
</section>
