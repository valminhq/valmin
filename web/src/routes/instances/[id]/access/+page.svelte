<script lang="ts">
	import { page } from '$app/state';
	import { ApiError } from '$lib/api/errors';
	import {
		grants,
		type Grant,
		type GrantPage,
		type GrantRole,
		type ReplaceGrant
	} from '$lib/api/grants';
	import { userAdmin } from '$lib/api/admin';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import type { User } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Select from '$lib/components/ui/select';
	import { Label } from '$lib/components/ui/label';
	import { unsaved } from '$lib/state/dirty.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ShieldCheck from '@lucide/svelte/icons/shield-check';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	type Draft = ReplaceGrant;
	type Conflict = { current: Grant | null; etag: string };

	const id = $derived(page.params.id!);
	let instance = $state<Instance | null>(null);
	let catalogue = $state<GrantPage | null>(null);
	let people = $state<User[]>([]);
	let drafts = $state<Record<string, Draft>>({});
	let conflicts = $state<Record<string, Conflict>>({});
	let loading = $state(true);
	let busy = $state<string | null>(null);
	let failure = $state<unknown>(null);
	let newUser = $state('');
	let newRole = $state<GrantRole>('viewer');
	let newPerms = $state<string[]>([]);

	const canManage = $derived(session.allowedGlobally().includes(actions.grantsManage));

	/** A draft differs from the grant it was seeded from. Permissions are compared as sets:
	 * ticking a box and unticking it is not an edit. */
	const changed = $derived(
		(catalogue?.items ?? []).some((grant) => {
			const draft = drafts[grant.user_id];
			if (!draft) return false;
			return (
				draft.role !== grant.role ||
				draft.perms.length !== grant.perms.length ||
				draft.perms.some((action) => !grant.perms.includes(action))
			);
		})
	);
	unsaved(() => changed);

	const availablePeople = $derived(
		people.filter((person) => !catalogue?.items.some((grant) => grant.user_id === person.id))
	);

	$effect(() => {
		void load(id);
	});

	async function load(instanceId: string) {
		loading = true;
		failure = null;
		try {
			[instance, catalogue, people] = await Promise.all([
				instances.get(instanceId),
				grants.list(instanceId),
				userAdmin.list()
			]);
			drafts = Object.fromEntries(
				catalogue.items.map((grant) => [
					grant.user_id,
					{ role: grant.role, perms: [...grant.perms] }
				])
			);
			conflicts = {};
			if (!availablePeople.some((person) => person.id === newUser)) {
				newUser = availablePeople[0]?.id ?? '';
			}
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function nameOf(userId: string): string {
		return people.find((person) => person.id === userId)?.username ?? userId;
	}

	function baseActions(role: GrantRole): string {
		return (
			catalogue?.roles.find((option) => option.role === role)?.allowed_actions.join(', ') ?? ''
		);
	}

	function setRole(userId: string, role: string) {
		drafts[userId].role = role as GrantRole;
	}

	function toggle(perms: string[], action: string, checked: boolean) {
		if (checked && !perms.includes(action)) perms.push(action);
		if (!checked) {
			const at = perms.indexOf(action);
			if (at >= 0) perms.splice(at, 1);
		}
	}

	async function rememberConflict(userId: string) {
		try {
			const latest = await grants.get(id, userId);
			conflicts[userId] = { current: latest.data, etag: latest.etag };
		} catch (err) {
			if (err instanceof ApiError && err.status === 404) {
				conflicts[userId] = { current: null, etag: '' };
				return;
			}
			throw err;
		}
	}

	async function save(grant: Grant, etag = grant.etag) {
		busy = grant.user_id;
		failure = null;
		try {
			await grants.replace(id, grant.user_id, drafts[grant.user_id], etag);
			await load(id);
			await session.refreshPermissions();
		} catch (err) {
			if (err instanceof ApiError && err.status === 412) {
				try {
					await rememberConflict(grant.user_id);
				} catch (refreshError) {
					failure = refreshError;
				}
			} else {
				failure = err;
			}
		} finally {
			busy = null;
		}
	}

	async function overwrite(grant: Grant) {
		const conflict = conflicts[grant.user_id];
		if (!conflict) return;
		if (conflict.current) {
			await save(grant, conflict.etag);
			return;
		}
		busy = grant.user_id;
		try {
			await grants.create(id, grant.user_id, drafts[grant.user_id]);
			await load(id);
			await session.refreshPermissions();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}

	async function remove(grant: Grant) {
		busy = grant.user_id;
		failure = null;
		try {
			await grants.remove(id, grant.user_id, grant.etag);
			await load(id);
			await session.refreshPermissions();
		} catch (err) {
			if (err instanceof ApiError && err.status === 412) await rememberConflict(grant.user_id);
			else failure = err;
		} finally {
			busy = null;
		}
	}

	async function createGrant() {
		if (!newUser) return;
		busy = newUser;
		failure = null;
		try {
			await grants.create(id, newUser, { role: newRole, perms: newPerms });
			newPerms = [];
			await load(id);
			await session.refreshPermissions();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}
</script>

<div class="mx-auto grid max-w-3xl gap-6 p-6">
	<header class="grid gap-1">
		<h2 class="text-2xl font-semibold tracking-tight">Panel access</h2>
		<p class="text-sm text-muted-foreground">
			Choose what each person can see and change on {instance?.name ?? 'this server'}.
		</p>
	</header>

	<Problem error={failure} />

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if !canManage}
		<p class="text-sm text-muted-foreground">Access management is not available to you.</p>
	{:else if catalogue}
		<section class="grid gap-3" aria-labelledby="current-access">
			<h2 id="current-access" class="text-lg font-semibold">People with access</h2>
			{#if catalogue.items.length === 0}
				<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
					Nobody has been given access to this server.
				</div>
			{/if}

			{#each catalogue.items as grant (grant.user_id)}
				{@const draft = drafts[grant.user_id]}
				{@const conflict = conflicts[grant.user_id]}
				{#if draft}
					<Card.Root>
						<Card.Header>
							<h3 class="font-semibold">{nameOf(grant.user_id)}</h3>
							<Card.Description
								>Access was last set {new Date(
									grant.granted_at
								).toLocaleString()}.</Card.Description
							>
						</Card.Header>
						<Card.Content class="grid gap-5">
							<div class="grid gap-2">
								<Label for={`role-${grant.user_id}`}>Base access</Label>
								<Select.Root
									type="single"
									value={draft.role}
									onValueChange={(role) => setRole(grant.user_id, role)}
								>
									<Select.Trigger id={`role-${grant.user_id}`}>{draft.role}</Select.Trigger>
									<Select.Content>
										{#each catalogue.roles as option (option.role)}
											<Select.Item value={option.role}>{option.role}</Select.Item>
										{/each}
									</Select.Content>
								</Select.Root>
								<p class="text-sm text-muted-foreground">
									Includes {baseActions(draft.role)}.
								</p>
							</div>

							<fieldset class="grid gap-3">
								<legend class="text-sm font-medium">Extra capabilities</legend>
								{#each catalogue.extra_capabilities as extra (extra.action)}
									<Label class="items-start gap-3 font-normal">
										<input
											type="checkbox"
											class="mt-1 size-4 accent-primary"
											checked={draft.perms.includes(extra.action)}
											onchange={(event) =>
												toggle(draft.perms, extra.action, event.currentTarget.checked)}
										/>
										<span
											><span class="font-mono text-xs">{extra.action}</span><br /><span
												class="text-xs text-muted-foreground">{extra.risk}</span
											></span
										>
									</Label>
								{/each}
							</fieldset>

							{#if conflict}
								<div
									class="grid gap-3 rounded-lg border border-l-4 border-l-destructive bg-muted/40 p-4"
								>
									<p class="text-sm">
										Someone else {conflict.current ? 'changed this access' : 'removed this access'} since
										you loaded it. Your edits are still here.
									</p>
									{#if conflict.current}
										<p class="text-sm text-muted-foreground">
											Current: {conflict.current.role}; {conflict.current.perms.join(', ') ||
												'no extras'}.
										</p>
									{/if}
									<div class="flex flex-wrap gap-2">
										<Button variant="outline" onclick={() => load(id)}
											>Discard all access edits</Button
										>
										<Button onclick={() => overwrite(grant)}>Overwrite saved access</Button>
									</div>
								</div>
							{/if}
						</Card.Content>
						<Card.Footer class="flex gap-2">
							<Button disabled={busy === grant.user_id} onclick={() => save(grant)}>
								<ShieldCheck /> Save access
							</Button>
							<Button
								variant="ghost"
								disabled={busy === grant.user_id}
								onclick={() => remove(grant)}
							>
								<Trash2 /> Revoke access
							</Button>
						</Card.Footer>
					</Card.Root>
				{/if}
			{/each}
		</section>

		<section class="grid gap-3" aria-labelledby="give-access">
			<h2 id="give-access" class="text-lg font-semibold">Give access</h2>
			{#if availablePeople.length === 0}
				<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
					Every user already has access assigned to this server.
				</div>
			{:else}
				<Card.Root>
					<Card.Content class="grid gap-5">
						<div class="grid gap-2">
							<Label for="new-user">User</Label>
							<Select.Root type="single" bind:value={newUser}>
								<Select.Trigger id="new-user">{nameOf(newUser)}</Select.Trigger>
								<Select.Content>
									{#each availablePeople as person (person.id)}
										<Select.Item value={person.id}>{person.username}</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						</div>
						<div class="grid gap-2">
							<Label for="new-role">Base access</Label>
							<Select.Root type="single" bind:value={newRole}>
								<Select.Trigger id="new-role">{newRole}</Select.Trigger>
								<Select.Content>
									{#each catalogue.roles as option (option.role)}
										<Select.Item value={option.role}>{option.role}</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						</div>
						<fieldset class="grid gap-3">
							<legend class="text-sm font-medium">Extra capabilities</legend>
							{#each catalogue.extra_capabilities as extra (extra.action)}
								<Label class="items-start gap-3 font-normal">
									<input
										type="checkbox"
										class="mt-1 size-4 accent-primary"
										checked={newPerms.includes(extra.action)}
										onchange={(event) =>
											toggle(newPerms, extra.action, event.currentTarget.checked)}
									/>
									<span
										><span class="font-mono text-xs">{extra.action}</span><br /><span
											class="text-xs text-muted-foreground">{extra.risk}</span
										></span
									>
								</Label>
							{/each}
						</fieldset>
					</Card.Content>
					<Card.Footer>
						<Button disabled={!newUser || busy === newUser} onclick={createGrant}
							>Give access</Button
						>
					</Card.Footer>
				</Card.Root>
			{/if}
		</section>
	{/if}
</div>
