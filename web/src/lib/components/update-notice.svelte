<script lang="ts">
	import { actions, instances, type Instance, type UpdateStatus } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import * as Alert from '$lib/components/ui/alert';
	import { Button } from '$lib/components/ui/button';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ArrowUpCircle from '@lucide/svelte/icons/arrow-up-circle';

	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let status = $state<UpdateStatus | null>(null);
	let failure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let jobRunning = $state(false);
	let confirming = $state(false);

	const allowed = $derived(session.allowed(instance.id));
	const canUpdate = $derived(allowed.includes(actions.gameUpdate));
	/** Null until a check has run. Rendered as "not checked yet" rather than as "up to date":
	 * no observation is not the same answer as a matching one. */
	const available = $derived(status?.update_available === true);

	$effect(() => {
		void load(instance.id);
	});

	async function load(id: string) {
		try {
			status = await instances.updateStatus(id);
		} catch (err) {
			failure = err;
		}
	}

	/** Why an update cannot start, or null. The daemon requires a stopped server: it replaces
	 * the whole server tree, which is the one thing that must never happen under a live
	 * process. */
	const blocked = $derived.by(() => {
		if (jobRunning) return 'An update is running.';
		if (instance.state !== 'stopped') return 'Stop this server to update it.';
		return null;
	});

	async function update() {
		failure = null;
		try {
			const job = await instances.updateGame(instance.id, instance.modded);
			jobId = job.job_id;
			jobRunning = true;
		} catch (err) {
			failure = err;
		}
	}

	function finished() {
		jobRunning = false;
		void load(instance.id);
		onchange?.();
	}
</script>

{#if available}
	<Alert.Root>
		<ArrowUpCircle />
		<Alert.Title>A newer game build is available</Alert.Title>
		<Alert.Description>
			<div class="grid gap-3">
				<p>
					This server runs build <span class="font-mono"
						>{status?.installed_build_id ?? 'unknown'}</span
					>; the public branch is on
					<span class="font-mono">{status?.public_build_id}</span>.
				</p>

				{#if instance.modded}
					<!--
						`03 §8`: nothing auto-updates a modded server. A new build can break every mod on
						it, and the panel has no way to know in advance which ones — so the operator is
						told and decides, and the schedule that exists for vanilla servers skips this one.
					-->
					<p data-testid="modded-notice">
						This server has mods. Updating installs the new build and puts the mods back on it, but
						a new build can break a mod in ways the panel cannot check for. Nothing updates this
						server on its own — a schedule skips it and records the skip.
					</p>
				{/if}

				<Problem error={failure} />

				{#if jobId}
					<!--
						F4. The update takes a pre-update archive, downloads the build, clones it and
						replays every installed package, and parks the server stopped afterwards rather
						than starting it (B7). All of that is the daemon's job to report.
					-->
					<div class="rounded-lg border p-4">
						<JobProgress {jobId} onfinish={finished} />
					</div>
				{/if}

				{#if canUpdate}
					<div class="flex flex-wrap items-center gap-3">
						<Button size="sm" disabled={blocked !== null} onclick={() => (confirming = true)}>
							Update the game
						</Button>
						<span class="text-sm text-muted-foreground">
							{blocked ??
								'The world is archived first, and this server is left stopped afterwards.'}
						</span>
					</div>
				{/if}
			</div>
		</Alert.Description>
	</Alert.Root>

	<!--
		F5. An update replaces the whole server tree, and on a modded server it also decides what
		happens to the mods — which is the confirmation the daemon refuses to run without.
	-->
	<DestructiveConfirm
		bind:open={confirming}
		name={instance.name}
		title="Update {instance.name}?"
		description="The server files are replaced with build {status?.public_build_id}. The world is archived first and is not touched by the update{instance.modded
			? ', and every installed mod is put back onto the new build'
			: ''}. This server stays stopped afterwards so you can check it before starting."
		confirmLabel="Update"
		onconfirm={update}
	/>
{/if}
