<script lang="ts">
	import { resolve } from '$app/paths';
	import { keyAdmin } from '$lib/api/admin';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import KeyRound from '@lucide/svelte/icons/key-round';

	let jobId = $state<string | null>(null);
	let running = $state(false);
	let failure = $state<unknown>(null);

	// Rendered from allowed_actions, never from a role name (F3). The daemon answers 404 to a
	// caller without it, so hiding the control matches what the endpoint reports.
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));

	async function rotate() {
		failure = null;
		running = true;
		try {
			jobId = (await keyAdmin.rotate()).job_id;
		} catch (err) {
			failure = err;
			running = false;
		}
	}
</script>

<main class="mx-auto grid max-w-2xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Encryption keys</h1>
		<p class="text-sm text-muted-foreground">
			Server passwords, RCON passwords and authentication secrets are encrypted with keys derived
			from the master key on disk.
		</p>
	</header>

	<Problem error={failure} />

	<Card.Root>
		<Card.Header>
			<Card.Title>Rotate derived keys</Card.Title>
			<Card.Description>
				Every stored secret is re-encrypted under a new key generation. Nothing has to be re-entered
				and no server is stopped.
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-4">
			<!--
				`10 §3.3`, Q26: the new generation derives from the same master key, so an operator
				reaching for this after a suspected leak has to be told what it does not do — before
				they press it, not in the result.
			-->
			<p class="rounded-lg border border-l-4 border-l-destructive bg-muted/40 px-4 py-3 text-sm">
				This does not replace the master key. If <code>secret.key</code> has leaked, rotating derived
				keys does not protect the secrets it covers: restore from a backup onto a host with a new master
				key and re-enter the passwords.
			</p>

			{#if allowed}
				<div class="flex flex-wrap items-center gap-3">
					<Button onclick={rotate} disabled={running}>
						<KeyRound /> Rotate derived keys
					</Button>
					<span class="text-sm text-muted-foreground">
						Interrupted halfway, rotating again finishes what is left rather than starting over.
					</span>
				</div>
			{/if}

			{#if jobId}
				<div class="rounded-lg border p-4">
					<JobProgress {jobId} onfinish={() => (running = false)} />
				</div>
			{/if}
		</Card.Content>
	</Card.Root>
</main>
