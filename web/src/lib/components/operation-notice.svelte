<script lang="ts">
	import { actions, type Instance } from '$lib/api/instances';
	import { operations, type Operation, type OperationStep } from '$lib/api/operations';
	import { session } from '$lib/state/session.svelte';
	import * as Alert from '$lib/components/ui/alert';
	import { Button } from '$lib/components/ui/button';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Check from '@lucide/svelte/icons/check';
	import Circle from '@lucide/svelte/icons/circle';
	import CircleDot from '@lucide/svelte/icons/circle-dot';
	import Hammer from '@lucide/svelte/icons/hammer';

	let {
		instance,
		operation,
		onchange
	}: { instance: Instance; operation: Operation | null; onchange?: () => void } = $props();

	let failure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let abandoning = $state(false);

	/** Resuming builds the instance, so it is the creation authority rather than this
	 * instance's own operator (`09 §3.3`). */
	const canResume = $derived(session.allowed(instance.id).includes(actions.create));

	/** What the step does, in the words the operator chose it in. `mod_install` is the only kind
	 * that appears more than once, which is what `ref` distinguishes. */
	function label(step: OperationStep): string {
		switch (step.kind) {
			case 'provision':
				return 'Install the server files';
			case 'mod_install':
				return `Install ${step.ref ?? 'a mod'}`;
			case 'config_apply':
				return 'Apply the imported configuration';
			case 'start':
				return 'Start the server';
			default:
				return step.kind;
		}
	}

	async function resume() {
		failure = null;
		try {
			jobId = (await operations.resume(instance.id)).job_id;
		} catch (err) {
			failure = err;
		}
	}

	async function abandon() {
		failure = null;
		try {
			await operations.abandon(instance.id);
			onchange?.();
		} catch (err) {
			failure = err;
		}
	}

	function finished() {
		jobId = null;
		onchange?.();
	}
</script>

<!--
	Q52: a chain cut mid-way leaves the instance stopped with its lock free, so nothing about the
	instance itself says the mods or the configuration it was defined with never landed. This is
	where that is said, and the only place the remaining steps can be asked for.
-->
{#if operation}
	{@const op = operation}
	{@const interrupted = op.state === 'interrupted'}
	<Alert.Root variant={interrupted ? 'destructive' : 'default'}>
		<Hammer />
		<Alert.Title>
			{interrupted ? 'Setup did not finish' : 'Setting this server up'}
		</Alert.Title>
		<Alert.Description>
			<div class="grid gap-3">
				<p>
					{#if interrupted}
						Setup was interrupted while {op.kind === 'import' ? 'importing' : 'creating'} this server.
						Resume setup to finish the remaining steps, or skip them to keep the partial setup. You must
						choose before starting the server.
					{:else}
						Setup continues automatically. The server starts afterwards only if you selected that
						option.
					{/if}
				</p>

				<ol class="grid gap-1.5">
					{#each op.steps as step, i (i)}
						{@const done = i < op.cursor}
						{@const current = i === op.cursor}
						<li class="flex items-center gap-2 text-sm">
							{#if done}
								<Check class="size-4 shrink-0" aria-hidden="true" />
							{:else if current}
								<CircleDot class="size-4 shrink-0" aria-hidden="true" />
							{:else}
								<Circle class="size-4 shrink-0 opacity-40" aria-hidden="true" />
							{/if}
							<span class={done ? '' : current ? 'font-medium' : 'opacity-60'}>{label(step)}</span>
							<span class="sr-only">
								{done ? 'done' : current ? 'next' : 'not started'}
							</span>
						</li>
					{/each}
				</ol>

				<Problem error={failure} />

				{#if jobId}
					<div class="rounded-lg border p-4">
						<JobProgress {jobId} onfinish={finished} />
					</div>
				{/if}

				{#if canResume && interrupted}
					<div class="flex flex-wrap gap-2">
						<Button size="sm" disabled={jobId !== null} onclick={resume}>Resume setup</Button>
						<Button
							variant="outline"
							size="sm"
							disabled={jobId !== null}
							onclick={() => (abandoning = true)}
						>
							Skip remaining steps
						</Button>
					</div>
				{/if}
			</div>
		</Alert.Description>
	</Alert.Root>

	<!--
		Stopping here is not a delete, but it is not undoable either: the steps that never ran are
		dropped, and the operator is left with a server that is not what they defined.
	-->
	<DestructiveConfirm
		bind:open={abandoning}
		name={instance.name}
		title="Skip the remaining setup for {instance.name}?"
		description="The remaining setup steps are permanently skipped. Existing files stay in place, but some requested mods or settings may be missing. You can start the server manually afterwards."
		confirmLabel="Skip remaining steps"
		onconfirm={abandon}
	/>
{/if}
