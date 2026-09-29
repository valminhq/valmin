<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { inviteAdmin, userAdmin, type Invite, type IssuedInvite } from '$lib/api/admin';
	import { grants, type GrantPage, type GrantRole } from '$lib/api/grants';
	import { instances, type Instance } from '$lib/api/instances';
	import type { User } from '$lib/api/types';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Select from '$lib/components/ui/select';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import { expiresIn, expiringSoon, inviteStatus, type InviteStatus } from '$lib/invite-status';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import CopyButton from '$lib/components/copy-button.svelte';
	import Link from '@lucide/svelte/icons/link';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	let invitations = $state<Invite[]>([]);
	let people = $state<User[]>([]);
	let servers = $state<Instance[]>([]);
	let vocabulary = $state<GrantPage | null>(null);
	let selectedInstance = $state('none');
	let grantRole = $state<GrantRole>('viewer');
	let grantPerms = $state<string[]>([]);
	let issued = $state<IssuedInvite | null>(null);
	let loading = $state(true);
	let busy = $state<string | null>(null);
	let failure = $state<unknown>(null);
	let filter = $state<InviteStatus | 'all'>('active');

	/** The clock statuses and expiries are measured against, refreshed every minute. */
	let now = $state(Date.now());
	$effect(() => {
		const tick = setInterval(() => (now = Date.now()), 60_000);
		return () => clearInterval(tick);
	});

	const filters: Array<[value: InviteStatus | 'all', label: string]> = [
		['active', 'Active'],
		['redeemed', 'Redeemed'],
		['revoked', 'Revoked'],
		['expired', 'Expired'],
		['all', 'All']
	];
	const shown = $derived(matching(filter));

	$effect(() => {
		void load().then(preselect);
	});

	// The role and extra-capability vocabulary is daemon-owned and the same for every
	// instance, so it is fetched once and reused. Only the selection resets.
	$effect(() => {
		const instanceId = selectedInstance;
		grantRole = 'viewer';
		grantPerms = [];
		if (instanceId !== 'none' && !vocabulary) void loadVocabulary(instanceId);
	});

	async function load() {
		loading = true;
		failure = null;
		try {
			[invitations, people, servers] = await Promise.all([
				inviteAdmin.list(),
				userAdmin.list(),
				instances.list()
			]);
			now = Date.now();
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	/** Selects the server a ?instance= link names in the create form, when the caller can see it. */
	function preselect() {
		const wanted = page.url.searchParams.get('instance');
		if (wanted && servers.some((server) => server.id === wanted)) selectedInstance = wanted;
	}

	async function loadVocabulary(instanceId: string) {
		try {
			vocabulary = await grants.list(instanceId);
		} catch (err) {
			failure = err;
		}
	}

	function toggle(action: string, checked: boolean) {
		if (checked && !grantPerms.includes(action)) grantPerms.push(action);
		if (!checked) grantPerms = grantPerms.filter((item) => item !== action);
	}

	async function issueInvite() {
		busy = 'issue';
		failure = null;
		issued = null;
		try {
			issued = await inviteAdmin.issue({
				instance_id: selectedInstance === 'none' ? null : selectedInstance,
				grant_role: selectedInstance === 'none' ? null : grantRole,
				grant_perms: selectedInstance === 'none' ? [] : grantPerms
			});
			await load();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}

	async function revoke(invite: Invite) {
		busy = invite.id;
		failure = null;
		try {
			await inviteAdmin.revoke(invite.id);
			await load();
		} catch (err) {
			failure = err;
		} finally {
			busy = null;
		}
	}

	function userName(id: string | null): string {
		if (!id) return 'Unknown user';
		return people.find((person) => person.id === id)?.username ?? id;
	}

	function instanceName(id: string | null): string {
		if (!id) return 'Panel access only';
		return servers.find((server) => server.id === id)?.name ?? id;
	}

	/** A timestamp in the viewer's locale. */
	function when(iso: string | null): string {
		return iso ? new Date(iso).toLocaleString() : 'an unknown time';
	}

	/** The loaded invites a filter shows. */
	function matching(value: InviteStatus | 'all'): Invite[] {
		if (value === 'all') return invitations;
		return invitations.filter((invite) => inviteStatus(invite, now) === value);
	}
</script>

<main class="mx-auto grid max-w-4xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Invites</h1>
		<p class="text-sm text-muted-foreground">
			Create a single-use link so someone can choose their own username and password.
		</p>
	</header>

	<Problem error={failure} />

	{#if issued}
		{@const credential = issued}
		<section
			class="grid gap-3 rounded-lg border border-l-4 border-l-primary bg-muted/40 p-4"
			aria-live="polite"
		>
			<div>
				<h2 class="font-semibold">Copy this invite now</h2>
				<p class="text-sm text-muted-foreground">
					The token is shown once and expires {new Date(credential.expires_at).toLocaleString()}.
				</p>
			</div>
			<code class="rounded bg-background px-3 py-2 text-sm break-all">{credential.url}</code>
			<div class="flex flex-wrap gap-2">
				<CopyButton value={credential.url} label="Copy link" />
				<CopyButton value={credential.token} label="Copy code" />
			</div>
		</section>
	{/if}

	<section class="grid gap-3" aria-labelledby="issue-invite">
		<h2 id="issue-invite" class="text-lg font-semibold">Create invite</h2>
		<Card.Root>
			<Card.Content class="grid gap-5">
				<div class="grid gap-2">
					<Label for="invite-instance">Initial server access</Label>
					<Select.Root type="single" bind:value={selectedInstance}>
						<Select.Trigger id="invite-instance">
							{selectedInstance === 'none' ? 'No server yet' : instanceName(selectedInstance)}
						</Select.Trigger>
						<Select.Content>
							<Select.Item value="none">No server yet</Select.Item>
							{#each servers as server (server.id)}
								<Select.Item value={server.id}>{server.name}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
					<p class="text-sm text-muted-foreground">
						An invite without a server creates a member with an empty dashboard.
					</p>
				</div>

				{#if selectedInstance !== 'none'}
					<div class="grid gap-2 sm:max-w-xs">
						<Label for="invite-role">Base access</Label>
						<Select.Root type="single" bind:value={grantRole} disabled={!vocabulary}>
							<Select.Trigger id="invite-role">{grantRole}</Select.Trigger>
							<Select.Content>
								{#each vocabulary?.roles ?? [] as option (option.role)}
									<Select.Item value={option.role}>{option.role}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					</div>

					<fieldset class="grid gap-3">
						<legend class="text-sm font-medium">Extra capabilities</legend>
						{#each vocabulary?.extra_capabilities ?? [] as extra (extra.action)}
							<Label class="items-start gap-3 font-normal">
								<input
									type="checkbox"
									class="mt-1 size-4 accent-primary"
									checked={grantPerms.includes(extra.action)}
									onchange={(event) => toggle(extra.action, event.currentTarget.checked)}
								/>
								<span>
									<span class="font-mono text-xs">{extra.action}</span><br />
									<span class="text-xs text-muted-foreground">{extra.risk}</span>
								</span>
							</Label>
						{/each}
					</fieldset>
				{/if}
			</Card.Content>
			<Card.Footer>
				<Button
					disabled={busy === 'issue' || (selectedInstance !== 'none' && !vocabulary)}
					onclick={issueInvite}
				>
					<Link /> Create invite
				</Button>
			</Card.Footer>
		</Card.Root>
	</section>

	<section class="grid gap-3" aria-labelledby="invite-history">
		<h2 id="invite-history" class="text-lg font-semibold">Invite history</h2>
		{#if loading && invitations.length === 0}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else if invitations.length === 0}
			<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
				No invites have been issued.
			</div>
		{:else}
			<div
				class="flex w-fit flex-wrap gap-1 rounded-lg bg-muted p-1"
				role="group"
				aria-label="Invite status"
			>
				{#each filters as [value, label] (value)}
					<Button
						variant="ghost"
						size="sm"
						aria-pressed={filter === value}
						class={[
							'rounded-md px-3 font-medium',
							filter === value ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground'
						]}
						onclick={() => (filter = value)}
					>
						{label} <span class="text-muted-foreground tabular-nums">{matching(value).length}</span>
					</Button>
				{/each}
			</div>
			{#if shown.length === 0}
				<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
					No {filters.find(([value]) => value === filter)?.[1].toLowerCase()} invites.
				</div>
			{/if}
		{/if}

		{#each shown as invite (invite.id)}
			{@const standing = inviteStatus(invite, now)}
			<Card.Root>
				<Card.Header>
					<Card.Title class="text-base">{instanceName(invite.instance_id)}</Card.Title>
					<Card.Description>
						Issued by {userName(invite.created_by)} on {when(invite.created_at)}
					</Card.Description>
					{#if standing === 'active'}
						<p
							class={[
								'text-sm',
								expiringSoon(invite.expires_at, now)
									? 'font-medium text-amber-800 dark:text-amber-300'
									: 'text-muted-foreground'
							]}
						>
							Expires {expiresIn(invite.expires_at, now)} ({when(invite.expires_at)})
						</p>
					{:else if standing === 'redeemed'}
						<p class="text-sm text-muted-foreground">
							Redeemed by {userName(invite.redeemed_by)} on {when(invite.redeemed_at)}
						</p>
					{:else if standing === 'revoked'}
						<p class="text-sm text-muted-foreground">Revoked on {when(invite.revoked_at)}</p>
					{:else}
						<p class="text-sm text-muted-foreground">Expired on {when(invite.expires_at)}</p>
					{/if}
				</Card.Header>
				{#if invite.grant_role}
					<Card.Content class="text-sm text-muted-foreground">
						{invite.grant_role} access{invite.grant_perms.length
							? ` with ${invite.grant_perms.join(', ')}`
							: ''}
					</Card.Content>
				{/if}
				{#if standing === 'active'}
					<Card.Footer>
						<Button variant="ghost" disabled={busy === invite.id} onclick={() => revoke(invite)}>
							<Trash2 /> Revoke invite
						</Button>
					</Card.Footer>
				{/if}
			</Card.Root>
		{/each}
	</section>
</main>
