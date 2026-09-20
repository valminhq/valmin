<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import type { Job } from '$lib/api/types';
	import { instanceList } from '$lib/state/instances.svelte';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Alert from '$lib/components/ui/alert';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Info from '@lucide/svelte/icons/info';

	const id = $derived(page.params.id ?? '');
	let source = $state<Instance | null>(null);
	let name = $state('');
	let job = $state<Job | null>(null);
	let failure = $state<unknown>(null);
	let busy = $state(false);

	const canClone = $derived(session.allowed(id).includes(actions.clone));
	const ready = $derived(canClone && source?.state === 'stopped' && name.trim() !== '' && !busy);

	$effect(() => {
		instances
			.get(id)
			.then((row) => {
				source = row;
				if (!name) name = `${row.name} copy`;
			})
			.catch((err) => (failure = err));
	});

	async function submit() {
		if (!ready) return;
		busy = true;
		failure = null;
		try {
			job = await instances.clone(id, name.trim());
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}

	async function finished(result: Job) {
		if (result.status !== 'succeeded' || !result.instance_id) return;
		await Promise.all([instanceList.load(), session.refreshPermissions()]);
		await goto(resolve('/instances/[id]', { id: result.instance_id }));
	}
</script>

<div class="mx-auto grid max-w-2xl gap-4 p-6">
	<h2 class="text-2xl font-semibold tracking-tight">Clone this server</h2>
	<Problem error={failure} />

	{#if !canClone}
		<p class="text-sm text-muted-foreground">Cloning servers is an administrator capability.</p>
	{:else if job}
		<Card.Root>
			<Card.Header>
				<Card.Title>Creating {name}</Card.Title>
				<Card.Description>
					The copy will stay stopped when its files and container are ready.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<JobProgress jobId={job.job_id} onfinish={finished} />
			</Card.Content>
		</Card.Root>
	{:else if source}
		{#if source.state !== 'stopped'}
			<Alert.Root variant="destructive">
				<Info />
				<Alert.Title>Stop {source.name} first</Alert.Title>
				<Alert.Description>
					Cloning never disconnects players or stops the source server for you.
				</Alert.Description>
			</Alert.Root>
		{/if}

		<Card.Root>
			<Card.Header>
				<Card.Title>What is copied</Card.Title>
				<Card.Description>
					The world, installed game build, mods, settings files, launch settings, and game password.
					The new server receives its own ports, identity, data directories, and stopped container.
				</Card.Description>
			</Card.Header>
			<Card.Content class="grid gap-4">
				<div class="grid gap-2">
					<Label for="clone-name">New panel name</Label>
					<Input id="clone-name" bind:value={name} autocomplete="off" />
				</div>
				<p class="text-sm text-muted-foreground">
					Users, access grants, and the source backup catalogue are not copied. Changes to either
					server after cloning do not affect the other.
				</p>
				<Button class="justify-self-start" disabled={!ready} onclick={submit}>
					{busy ? 'Cloning…' : 'Clone server'}
				</Button>
			</Card.Content>
		</Card.Root>
	{/if}
</div>
