<script lang="ts">
	import { NativeSelect } from '#lib/components/ui/native-select/index.js';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { canCompare, switchServerPath } from '#lib/nav.js';
	import { session } from '#lib/state/session.svelte.js';
	import { instanceList } from '#lib/state/instances.svelte.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { topics, type ServerMessage } from '#lib/socket/messages.js';
	import ServerNav from '#lib/components/server-nav.svelte';
	import StateBadge from '#lib/components/state-badge.svelte';
	import ServerControls from '#lib/components/server-controls.svelte';
	import Problem from '#lib/components/problem.svelte';
	import GitCompare from '@lucide/svelte/icons/git-compare';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { instances } from '#lib/api/instances.js';
	import { operations, type Operation } from '#lib/api/operations.js';
	import type { Job } from '#lib/api/types.js';
	import { jobLabel } from '#lib/job-history.js';

	let { children } = $props();

	const id = $derived(page.params.id ?? '');
	// A deep link lands here without the dashboard ever having run, so the list is loaded on
	// demand. Only the name and state are read from it; each section fetches its own row.
	$effect(() => {
		if (session.user) void instanceList.ensure();
	});
	const instance = $derived(instanceList.items.find((row) => row.id === id) ?? null);
	// The socket cannot say what changed while it was gone, so a reconnect re-reads the list.
	$effect(() => socket.onConnected(() => void instanceList.load()));

	// Whether this caller's list holds the server. One it cannot see is never read here, so a
	// stranger's link stops at the section's own 404.
	const listed = $derived(instanceList.items.some((row) => row.id === id));

	// The job running on this server and the setup steps it still owes, read once, then on each
	// state message, on reconnect and on a slow timer. Only the newest reply is shown.
	let working = $state<Job | null>(null);
	let owed = $state<Operation | null>(null);
	$effect(() => {
		if (!session.user || !id || !listed) return;
		let current = true;
		let latest = 0;
		async function read() {
			if (document.hidden) return;
			const seq = ++latest;
			try {
				const [jobs, operation] = await Promise.all([instances.jobs(id), operations.get(id)]);
				if (!current || seq !== latest) return;
				working = jobs.find((job) => job.status === 'running') ?? null;
				owed = operation;
			} catch {
				// Each section reports its own failed reads; the header only goes quiet.
			}
		}
		const off = socket.subscribe(topics.state(id), (m: ServerMessage) => {
			if (m.type === 'state') void read();
		});
		void read();
		const unhook = socket.onConnected(() => void read());
		const timer = setInterval(read, 60_000);
		document.addEventListener('visibilitychange', read);
		return () => {
			current = false;
			off();
			unhook();
			clearInterval(timer);
			document.removeEventListener('visibilitychange', read);
		};
	});
	$effect(() => {
		void id;
		working = null;
		owed = null;
	});

	// A refused request is about one server in one state; it goes when either changes.
	let failure = $state<unknown>(null);
	$effect(() => {
		void id;
		void instance?.state;
		failure = null;
	});

	const compareHref = $derived(resolve('/instances/[id]/compare', { id }));
	const offerCompare = $derived(
		canCompare(session.allowed(id)) && page.url.pathname !== compareHref
	);

	// Keeps the section being viewed when the chosen server offers it to this caller.
	function switchTo(event: Event & { currentTarget: HTMLSelectElement }) {
		const target = event.currentTarget.value;
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		void goto(switchServerPath(page.url.pathname, id, target, session.allowed(target)));
	}
</script>

<!--
	Which server is being changed is settled once, above every section, rather than restated
	differently by each of them. The sections are h2 below this.
-->
{#if session.user}
	<div class="border-b bg-card">
		<div class="mx-auto grid max-w-6xl gap-3 px-4 pt-4 sm:px-6">
			<div class="flex flex-wrap items-center gap-3">
				<h1 class="min-w-0 text-2xl font-semibold tracking-tight wrap-anywhere">
					{instance?.name ?? 'Server'}
				</h1>
				{#if instance}
					<StateBadge
						state={instance.state}
						restartRequired={instance.restart_required}
						pendingRestart={instance.pending_restart}
					/>
					{#key id}
						<ServerControls
							{instance}
							allowed={session.allowed(id)}
							owes={owed !== null}
							onfailure={(error) => (failure = error)}
						/>
					{/key}
					<p class="text-sm text-muted-foreground" role="status">
						{#if working}
							<a
								class="inline-flex items-center gap-1.5 hover:text-foreground hover:underline"
								href={resolve('/instances/[id]', { id })}
							>
								<LoaderCircle class="size-4 motion-safe:animate-spin" aria-hidden="true" />
								{jobLabel(working.kind, working.changes)}
								running…
							</a>
						{:else if owed}
							<a
								class="hover:text-foreground hover:underline"
								href={resolve('/instances/[id]', { id })}
							>
								Setup is unfinished. See the overview.
							</a>
						{/if}
					</p>
				{/if}
				<div class="flex w-full items-center gap-x-4 gap-y-2 sm:ml-auto sm:w-auto">
					{#if instance && instanceList.items.length > 1}
						<label class="sr-only" for="server-switcher">Switch server</label>
						<NativeSelect
							id="server-switcher"
							class="min-w-0 flex-1 sm:max-w-64 sm:flex-none"
							value={id}
							onchange={switchTo}
						>
							{#each instanceList.items as row (row.id)}
								<option value={row.id}>{row.name}</option>
							{/each}
						</NativeSelect>
					{/if}
					{#if offerCompare}
						<a
							class="inline-flex min-h-6 min-w-6 items-center justify-center gap-1.5 text-sm text-muted-foreground hover:text-foreground hover:underline"
							href={compareHref}
						>
							<GitCompare class="size-4" aria-hidden="true" />
							<span class="sr-only sm:not-sr-only">Compare with another server</span>
						</a>
					{/if}
				</div>
			</div>
			<Problem error={failure} />
		</div>
		<ServerNav {id} />
	</div>
{/if}

<!-- Keyed by server, so a section never keeps one server's data while addressing another. -->
<main class="mx-auto max-w-6xl px-4 py-6 sm:px-6">
	{#key id}
		{@render children()}
	{/key}
</main>
