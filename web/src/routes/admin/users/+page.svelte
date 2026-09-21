<script lang="ts">
	import { resolve } from '$app/paths';
	import { userAdmin } from '$lib/api/admin';
	import type { Role, User } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import CopyButton from '$lib/components/copy-button.svelte';
	import KeyRound from '@lucide/svelte/icons/key-round';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import UserPlus from '@lucide/svelte/icons/user-plus';

	let people = $state<User[]>([]);
	let loading = $state(true);
	let busy = $state<string | null>(null);
	let failure = $state<unknown>(null);
	let username = $state('');
	let role = $state<Role>('member');
	let credential = $state<{ username: string; password: string } | null>(null);
	let deleting = $state<User | null>(null);
	let deleteOpen = $state(false);

	$effect(() => {
		void load();
	});

	async function load() {
		loading = true;
		failure = null;
		try {
			people = await userAdmin.list();
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function generatedPassword(): string {
		const bytes = crypto.getRandomValues(new Uint8Array(18));
		return btoa(String.fromCharCode(...bytes))
			.replaceAll('+', '-')
			.replaceAll('/', '_');
	}

	async function createUser(event: SubmitEvent) {
		event.preventDefault();
		const password = generatedPassword();
		busy = 'create';
		failure = null;
		credential = null;
		try {
			await userAdmin.create({ username, password, role });
			credential = { username, password };
			username = '';
			role = 'member';
			await load();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}

	async function updateUser(person: User, patch: { role?: Role; disabled?: boolean }) {
		busy = person.id;
		failure = null;
		credential = null;
		try {
			await userAdmin.update(person.id, patch);
			await load();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}

	async function resetPassword(person: User) {
		busy = person.id;
		failure = null;
		credential = null;
		try {
			const result = await userAdmin.resetPassword(person.id);
			credential = { username: person.username, password: result.password };
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}

	function askToDelete(person: User) {
		deleting = person;
		deleteOpen = true;
	}

	async function removeUser(person: User) {
		busy = person.id;
		failure = null;
		credential = null;
		try {
			await userAdmin.remove(person.id);
			await load();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}
</script>

<main class="mx-auto grid max-w-4xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Users</h1>
		<p class="text-sm text-muted-foreground">
			Create accounts, change panel roles, and revoke access to the panel.
		</p>
	</header>

	<Problem error={failure} />

	{#if credential}
		<section
			class="grid gap-3 rounded-lg border border-l-4 border-l-primary bg-muted/40 p-4"
			aria-live="polite"
		>
			<div>
				<h2 class="font-semibold">Copy this password now</h2>
				<p class="text-sm text-muted-foreground">
					This is the only time Valmin shows the password for {credential.username}.
				</p>
			</div>
			<div class="flex flex-wrap items-center gap-2">
				<code class="min-w-0 rounded bg-background px-3 py-2 text-sm break-all"
					>{credential.password}</code
				>
				<CopyButton value={credential.password} label="Copy password" />
			</div>
		</section>
	{/if}

	<section class="grid gap-3" aria-labelledby="create-user">
		<h2 id="create-user" class="text-lg font-semibold">Create user</h2>
		<Card.Root>
			<form onsubmit={createUser}>
				<Card.Content class="grid gap-4 sm:grid-cols-[1fr_12rem_auto] sm:items-end">
					<div class="grid gap-2">
						<Label for="username">Username</Label>
						<Input id="username" autocomplete="off" required bind:value={username} />
					</div>
					<div class="grid gap-2">
						<Label for="new-role">Panel role</Label>
						<Select.Root type="single" bind:value={role}>
							<Select.Trigger id="new-role">{role}</Select.Trigger>
							<Select.Content>
								<Select.Item value="member">member</Select.Item>
								<Select.Item value="admin">admin</Select.Item>
							</Select.Content>
						</Select.Root>
					</div>
					<Button type="submit" disabled={!username || busy === 'create'}>
						<UserPlus /> Create user
					</Button>
				</Card.Content>
			</form>
		</Card.Root>
	</section>

	<section class="grid gap-3" aria-labelledby="existing-users">
		<h2 id="existing-users" class="text-lg font-semibold">Existing users</h2>
		{#if loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if people.length === 0}
			<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
				No users exist.
			</div>
		{/if}

		{#each people as person (person.id)}
			<Card.Root>
				<Card.Header>
					<Card.Title class="flex flex-wrap items-center gap-2">
						{person.username}
						{#if person.id === session.user?.id}
							<span class="text-xs font-normal text-muted-foreground">You</span>
						{/if}
						{#if person.owner}
							<span class="text-xs font-normal text-muted-foreground">Owner</span>
						{/if}
						{#if person.disabled}
							<span class="text-xs font-normal text-destructive">Disabled</span>
						{/if}
					</Card.Title>
					<Card.Description>
						Created {new Date(person.created_at).toLocaleString()} · {person.last_login_at
							? `Last signed in ${new Date(person.last_login_at).toLocaleString()}`
							: 'Never signed in'}
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-2 sm:max-w-xs">
					{#if person.owner}
						<p class="text-sm text-muted-foreground">
							The owner is always an admin, and cannot be disabled or deleted, so this panel can
							never be left without one.
						</p>
					{:else}
						<Label for={`role-${person.id}`}>Panel role</Label>
						<Select.Root
							type="single"
							value={person.role}
							disabled={busy === person.id}
							onValueChange={(next) => updateUser(person, { role: next as Role })}
						>
							<Select.Trigger id={`role-${person.id}`}>{person.role}</Select.Trigger>
							<Select.Content>
								<Select.Item value="member">member</Select.Item>
								<Select.Item value="admin">admin</Select.Item>
							</Select.Content>
						</Select.Root>
					{/if}
				</Card.Content>
				<Card.Footer class="flex flex-wrap gap-2">
					{#if !person.owner}
						<Button
							variant="outline"
							disabled={busy === person.id}
							onclick={() => updateUser(person, { disabled: !person.disabled })}
						>
							{person.disabled ? 'Enable sign-in' : 'Disable sign-in'}
						</Button>
					{/if}
					<Button
						variant="outline"
						disabled={busy === person.id}
						onclick={() => resetPassword(person)}
					>
						<KeyRound /> Reset password
					</Button>
					{#if !person.owner}
						<Button
							variant="ghost"
							class="sm:ml-auto"
							disabled={busy === person.id}
							onclick={() => askToDelete(person)}
						>
							<Trash2 /> Delete
						</Button>
					{/if}
				</Card.Footer>
			</Card.Root>
		{/each}
	</section>
</main>

{#if deleting}
	{@const target = deleting}
	<DestructiveConfirm
		bind:open={deleteOpen}
		name={target.username}
		title="Delete {target.username}?"
		confirmLabel="Delete user"
		description="Their sessions, server grants, and issued invites are removed. Schedules remain and show an unnamed author."
		onconfirm={() => removeUser(target)}
	/>
{/if}
