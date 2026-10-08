<script lang="ts">
	import { formatInstant } from '$lib/api/schedules';
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { actionLabel, capitalise, describeAccess } from '$lib/access';
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
	import { adminRole, type User } from '$lib/api/types';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
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
	const canInvite = $derived(session.allowedGlobally().includes(actions.invitesManage));

	/** A draft differs from the grant it was seeded from. Permissions are compared as sets:
	 * ticking a box and unticking it is not an edit. */
	function edited(grant: Grant, draft: Draft | undefined): boolean {
		return (
			!!draft &&
			(draft.role !== grant.role ||
				draft.perms.length !== grant.perms.length ||
				draft.perms.some((action) => !grant.perms.includes(action)))
		);
	}

	const changed = $derived(
		(catalogue?.items ?? []).some((grant) => edited(grant, drafts[grant.user_id]))
	);
	unsaved(() => changed);

	const admins = $derived(people.filter((person) => person.role === adminRole && !person.disabled));
	const availablePeople = $derived(
		people.filter(
			(person) =>
				person.role !== adminRole && !catalogue?.items.some((grant) => grant.user_id === person.id)
		)
	);
	/** Who the Give access form is aimed at: the chosen person, or the first one who can still
	 * be given access. */
	const target = $derived(
		availablePeople.find((person) => person.id === newUser)?.id ?? availablePeople[0]?.id ?? ''
	);

	$effect(() => {
		void load(id);
	});

	/** A draft that starts equal to the grant. */
	function seed(grant: Grant): Draft {
		return { role: grant.role, perms: [...grant.perms] };
	}

	/** Loads this server's grants and the people list, and resets every draft and conflict. */
	async function load(instanceId: string) {
		loading = true;
		failure = null;
		try {
			[instance, catalogue, people] = await Promise.all([
				instances.get(instanceId),
				grants.list(instanceId),
				userAdmin.list()
			]);
			drafts = Object.fromEntries(catalogue.items.map((grant) => [grant.user_id, seed(grant)]));
			conflicts = {};
			newPerms = [];
			const wanted = page.url.searchParams.get('user') ?? '';
			if (availablePeople.some((person) => person.id === wanted)) newUser = wanted;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	/** Puts a grant the daemon returned into the list and reseeds only its draft. Other people's
	 * grants, drafts and conflicts stay exactly as they were loaded. */
	function adopt(grant: Grant) {
		if (!catalogue || grant.instance_id !== id) return;
		const at = catalogue.items.findIndex((item) => item.user_id === grant.user_id);
		if (at >= 0) catalogue.items[at] = grant;
		else catalogue.items.push(grant);
		drafts[grant.user_id] = seed(grant);
		delete conflicts[grant.user_id];
	}

	/** Drops a revoked grant with its draft and conflict, leaving everyone else's alone. */
	function forget(grant: Grant) {
		if (!catalogue || grant.instance_id !== id) return;
		catalogue.items = catalogue.items.filter((item) => item.user_id !== grant.user_id);
		delete drafts[grant.user_id];
		delete conflicts[grant.user_id];
	}

	function nameOf(userId: string): string {
		return people.find((person) => person.id === userId)?.username ?? userId;
	}

	/** Whether the account is an administrator, whose access a grant cannot change. */
	function holdsAdmin(userId: string): boolean {
		return people.some((person) => person.id === userId && person.role === adminRole);
	}

	/** The actions a base role carries, as the daemon lists them. */
	function baseActions(role: GrantRole): string[] {
		return catalogue?.roles.find((option) => option.role === role)?.allowed_actions ?? [];
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

	/** A stale write becomes a conflict for that person to resolve; anything else is shown above. */
	async function report(err: unknown, userId: string) {
		if (!(err instanceof ApiError && err.status === 412)) {
			failure = err;
			return;
		}
		try {
			await rememberConflict(userId);
		} catch (refreshError) {
			failure = refreshError;
		}
	}

	async function save(grant: Grant, etag = grant.etag) {
		busy = grant.user_id;
		failure = null;
		try {
			const saved = await grants.replace(id, grant.user_id, drafts[grant.user_id], etag);
			adopt(saved.data);
			await session.refreshPermissions();
		} catch (err) {
			await report(err, grant.user_id);
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
			const created = await grants.create(id, grant.user_id, drafts[grant.user_id]);
			adopt(created.data);
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
			forget(grant);
			await session.refreshPermissions();
		} catch (err) {
			await report(err, grant.user_id);
		} finally {
			busy = null;
		}
	}

	async function createGrant() {
		const userId = target;
		if (!userId) return;
		busy = userId;
		failure = null;
		try {
			const created = await grants.create(id, userId, { role: newRole, perms: newPerms });
			adopt(created.data);
			newPerms = [];
			await session.refreshPermissions();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}
</script>

{#snippet extras(perms: string[])}
	<fieldset class="grid gap-3">
		<legend class="text-sm font-medium">Extra capabilities</legend>
		{#each catalogue?.extra_capabilities ?? [] as extra (extra.action)}
			<Label class="items-start gap-3 font-normal">
				<input
					type="checkbox"
					class="mt-1 size-4 accent-primary"
					checked={perms.includes(extra.action)}
					onchange={(event) => toggle(perms, extra.action, event.currentTarget.checked)}
				/>
				<span class="grid gap-1">
					<span>{capitalise(actionLabel(extra.action))}</span>
					<span class="text-xs text-muted-foreground">{extra.risk}</span>
				</span>
			</Label>
		{/each}
	</fieldset>
{/snippet}

{#snippet effective(role: GrantRole, perms: string[])}
	<p class="text-sm" aria-live="polite">
		<span class="font-medium">Effective access:</span>
		{describeAccess([...baseActions(role), ...perms])}
	</p>
{/snippet}

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
			<p class="text-sm text-muted-foreground">
				Grants given on this server. Administrators are listed separately below.
			</p>
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
								>Access was last set {formatInstant(grant.granted_at)}.</Card.Description
							>
							{#if holdsAdmin(grant.user_id)}
								<p class="text-sm text-muted-foreground">
									This person is an administrator, so this grant changes nothing.
								</p>
							{/if}
						</Card.Header>
						<Card.Content class="grid gap-5">
							<div class="grid gap-2">
								<Label for={`role-${grant.user_id}`}>Base access</Label>
								<Select.Root
									type="single"
									value={draft.role}
									onValueChange={(role) => setRole(grant.user_id, role)}
								>
									<Select.Trigger id={`role-${grant.user_id}`}
										>{capitalise(draft.role)}</Select.Trigger
									>
									<Select.Content>
										{#each catalogue.roles as option (option.role)}
											<Select.Item value={option.role}>{capitalise(option.role)}</Select.Item>
										{/each}
									</Select.Content>
								</Select.Root>
								<p class="text-sm text-muted-foreground">
									{describeAccess(baseActions(draft.role))}
								</p>
							</div>

							{@render extras(draft.perms)}
							{@render effective(draft.role, draft.perms)}

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
											Saved access: {capitalise(conflict.current.role)}.
											{describeAccess([
												...baseActions(conflict.current.role),
												...conflict.current.perms
											])}
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
						<Card.Footer class="flex flex-wrap items-center gap-2">
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
							{#if edited(grant, draft)}
								<span class="text-sm text-muted-foreground">Unsaved changes</span>
							{/if}
						</Card.Footer>
					</Card.Root>
				{/if}
			{/each}
		</section>

		<section class="grid gap-3" aria-labelledby="give-access">
			<h2 id="give-access" class="text-lg font-semibold">Give access</h2>
			{#if canInvite}
				<a
					class="justify-self-start text-sm underline hover:text-foreground"
					href={resolve(`/admin/invites?instance=${encodeURIComponent(id)}`)}
				>
					Invite someone new to this server
				</a>
			{/if}
			{#if availablePeople.length === 0}
				<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
					There is nobody to give access to: administrators already have it and every other user has
					a grant.
				</div>
			{:else}
				<Card.Root>
					<Card.Content class="grid gap-5">
						<div class="grid gap-2">
							<Label for="new-user">User</Label>
							<Select.Root type="single" value={target} onValueChange={(next) => (newUser = next)}>
								<Select.Trigger id="new-user">{nameOf(target)}</Select.Trigger>
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
								<Select.Trigger id="new-role">{capitalise(newRole)}</Select.Trigger>
								<Select.Content>
									{#each catalogue.roles as option (option.role)}
										<Select.Item value={option.role}>{capitalise(option.role)}</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
							<p class="text-sm text-muted-foreground">
								{describeAccess(baseActions(newRole))}
							</p>
						</div>
						{@render extras(newPerms)}
						{@render effective(newRole, newPerms)}
					</Card.Content>
					<Card.Footer>
						<Button disabled={!target || busy === target} onclick={createGrant}>Give access</Button>
					</Card.Footer>
				</Card.Root>
			{/if}
		</section>

		<section class="grid gap-3" aria-labelledby="administrators">
			<h2 id="administrators" class="text-lg font-semibold">Administrators</h2>
			<p class="text-sm text-muted-foreground">
				Administrators can use every server without a grant. Their access cannot be limited here.
			</p>
			<ul class="flex flex-wrap gap-2">
				{#each admins as admin (admin.id)}
					<li><Badge variant="secondary">{admin.username}</Badge></li>
				{/each}
			</ul>
		</section>
	{/if}
</div>
