<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { inviteAdmin } from '$lib/api/admin';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';

	const token = $derived(page.params.token!);
	let username = $state('');
	let password = $state('');
	let confirmation = $state('');
	let busy = $state(false);
	let failure = $state<unknown>(null);
	let localError = $state('');

	async function redeem(event: SubmitEvent) {
		event.preventDefault();
		localError = '';
		failure = null;
		if (password !== confirmation) {
			localError = 'The passwords do not match.';
			return;
		}

		busy = true;
		try {
			const user = await inviteAdmin.redeem(token, username, password);
			password = '';
			confirmation = '';
			session.signedIn(user);
			await goto(resolve('/'), { replaceState: true });
		} catch (err) {
			failure = err;
			password = '';
			confirmation = '';
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Redeem invite · Valmin</title>
</svelte:head>

<main class="flex min-h-screen items-center justify-center p-6">
	<Card.Root class="w-full max-w-sm">
		<Card.Header>
			<Card.Title>Join Valmin</Card.Title>
			<Card.Description>Choose the credentials you will use to sign in.</Card.Description>
		</Card.Header>
		<form onsubmit={redeem}>
			<Card.Content class="grid gap-4">
				<Problem error={failure} />
				{#if localError}
					<p class="text-sm text-destructive" role="alert">{localError}</p>
				{/if}
				<div class="grid gap-2">
					<Label for="username">Username</Label>
					<Input id="username" autocomplete="username" required bind:value={username} />
				</div>
				<div class="grid gap-2">
					<Label for="password">Password</Label>
					<Input
						id="password"
						type="password"
						autocomplete="new-password"
						minlength={8}
						required
						bind:value={password}
					/>
				</div>
				<div class="grid gap-2">
					<Label for="confirmation">Confirm password</Label>
					<Input
						id="confirmation"
						type="password"
						autocomplete="new-password"
						minlength={8}
						required
						bind:value={confirmation}
					/>
				</div>
			</Card.Content>
			<Card.Footer class="mt-4">
				<Button type="submit" class="w-full" disabled={busy}>
					{busy ? 'Creating account…' : 'Create account'}
				</Button>
			</Card.Footer>
		</form>
	</Card.Root>
</main>
