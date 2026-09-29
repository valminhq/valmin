<script lang="ts">
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { ApiError } from '$lib/api/errors';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';

	let current = $state('');
	let next = $state('');
	let repeat = $state('');
	let busy = $state(false);
	let changed = $state(false);
	let failure = $state<unknown>(null);

	const mismatch = $derived(repeat !== '' && repeat !== next);
	const ready = $derived(current !== '' && next !== '' && next === repeat);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (!ready) return;
		busy = true;
		changed = false;
		failure = null;
		try {
			await api.post('/me/password', { current_password: current, new_password: next });
			current = next = repeat = '';
			changed = true;
		} catch (err) {
			failure = err;
			if (err instanceof ApiError && err.code === 'invalid_credentials') current = '';
		} finally {
			busy = false;
		}
	}
</script>

<main class="mx-auto grid max-w-2xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Change password</h1>
		<p class="text-sm text-muted-foreground">
			Choose a new password{session.user ? ` for ${session.user.username}` : ''}. Every other place
			you are signed in is signed out; this one stays signed in.
		</p>
	</header>

	<Problem error={failure} />

	{#if changed}
		<section
			class="grid gap-1 rounded-lg border border-l-4 border-l-primary bg-muted/40 p-4"
			role="status"
		>
			<h2 class="font-semibold">Password changed</h2>
			<p class="text-sm text-muted-foreground">Use the new password the next time you sign in.</p>
		</section>
	{/if}

	<Card.Root>
		<form onsubmit={submit}>
			<Card.Content class="grid gap-4">
				<div class="grid gap-2">
					<Label for="current-password">Current password</Label>
					<Input
						id="current-password"
						type="password"
						autocomplete="current-password"
						bind:value={current}
					/>
				</div>
				<div class="grid gap-2">
					<Label for="new-password">New password</Label>
					<Input id="new-password" type="password" autocomplete="new-password" bind:value={next} />
				</div>
				<div class="grid gap-2">
					<Label for="repeat-password">Confirm new password</Label>
					<Input
						id="repeat-password"
						type="password"
						autocomplete="new-password"
						aria-invalid={mismatch}
						aria-describedby={mismatch ? 'repeat-mismatch' : undefined}
						bind:value={repeat}
					/>
					{#if mismatch}
						<p id="repeat-mismatch" class="text-sm text-destructive">The passwords do not match.</p>
					{/if}
				</div>
			</Card.Content>
			<Card.Footer class="mt-4">
				<Button type="submit" disabled={busy || !ready}>
					{busy ? 'Changing…' : 'Change password'}
				</Button>
			</Card.Footer>
		</form>
	</Card.Root>
</main>
