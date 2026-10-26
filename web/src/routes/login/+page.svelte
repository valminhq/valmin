<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '#lib/api/client.js';
	import { leave, returnPath } from '#lib/nav.js';
	import { ApiError } from '#lib/api/errors.js';
	import type { User } from '#lib/api/types.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import Problem from '#lib/components/problem.svelte';

	let username = $state('');
	let password = $state('');
	let busy = $state(false);
	let failure = $state<unknown>(null);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		failure = null;
		try {
			const user = await api.post<User>('/auth/login', { username, password });
			session.signedIn(user);
			// Back to whatever was being opened when the session ran out. A destination this
			// user may not see resolves to its own 404, which is the honest answer. goto refuses
			// a path that matches no route, so that one is opened with a full page load.
			const path = returnPath(page.url);
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			await goto(path).catch(() => leave(path));
		} catch (err) {
			failure = err;
			// A wrong password should not sit in the box waiting to be submitted again.
			if (err instanceof ApiError && err.code === 'invalid_credentials') password = '';
		} finally {
			busy = false;
		}
	}
</script>

<main class="flex min-h-screen items-center justify-center p-6">
	<Card.Root class="w-full max-w-sm">
		<Card.Header>
			<Card.Title>Sign in</Card.Title>
			<Card.Description>Valmin</Card.Description>
		</Card.Header>
		<form onsubmit={submit}>
			<Card.Content class="grid gap-4">
				<Problem error={failure} />
				<div class="grid gap-2">
					<Label for="username">Username</Label>
					<Input id="username" name="username" autocomplete="username" bind:value={username} />
				</div>
				<div class="grid gap-2">
					<Label for="password">Password</Label>
					<Input
						id="password"
						name="password"
						type="password"
						autocomplete="current-password"
						bind:value={password}
					/>
				</div>
			</Card.Content>
			<Card.Footer class="mt-4">
				<Button type="submit" class="w-full" disabled={busy}>
					{busy ? 'Signing in…' : 'Sign in'}
				</Button>
			</Card.Footer>
		</form>
	</Card.Root>
</main>
