<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { ApiError } from '$lib/api/errors';
	import type { User } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';

	let token = $state('');
	let username = $state('');
	let password = $state('');
	let busy = $state(false);
	let failure = $state<unknown>(null);

	const fieldError = $derived(failure instanceof ApiError ? failure : null);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		failure = null;
		try {
			const user = await api.post<User>('/setup', { token, username, password });
			session.signedIn(user);
			await goto(resolve('/'));
		} catch (err) {
			failure = err;
		} finally {
			busy = false;
		}
	}
</script>

<main class="flex min-h-screen items-center justify-center p-6">
	<Card.Root class="w-full max-w-md">
		<Card.Header>
			<Card.Title>Set up this panel</Card.Title>
			<Card.Description>
				The daemon prints a one-time setup token to its own output, on every start until it is used.
				Paste it here to create the first administrator.
			</Card.Description>
		</Card.Header>
		<form onsubmit={submit}>
			<Card.Content class="grid gap-4">
				<Problem error={failure} />
				<div class="grid gap-2">
					<Label for="token">Setup token</Label>
					<Input id="token" name="token" bind:value={token} />
					{#if fieldError?.field('token')}
						<p class="text-sm text-destructive">{fieldError.field('token')}</p>
					{/if}
				</div>
				<div class="grid gap-2">
					<Label for="username">Username</Label>
					<Input id="username" name="username" autocomplete="username" bind:value={username} />
					{#if fieldError?.field('username')}
						<p class="text-sm text-destructive">{fieldError.field('username')}</p>
					{/if}
				</div>
				<div class="grid gap-2">
					<Label for="password">Password</Label>
					<Input
						id="password"
						name="password"
						type="password"
						autocomplete="new-password"
						bind:value={password}
					/>
					{#if fieldError?.field('password')}
						<p class="text-sm text-destructive">{fieldError.field('password')}</p>
					{/if}
				</div>
			</Card.Content>
			<Card.Footer class="mt-4">
				<Button type="submit" class="w-full" disabled={busy}>
					{busy ? 'Creating…' : 'Create administrator'}
				</Button>
			</Card.Footer>
		</form>
	</Card.Root>
</main>
