<script lang="ts">
	import { resolve } from '$app/paths';
	import { auditLog, userAdmin, type AuditEntry, type AuditFilter } from '$lib/api/admin';
	import { instances, type Instance } from '$lib/api/instances';
	import type { User } from '$lib/api/types';
	import { Button } from '$lib/components/ui/button';
	import * as Select from '$lib/components/ui/select';
	import { Label } from '$lib/components/ui/label';
	import Problem from '$lib/components/problem.svelte';
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';

	const any = 'any';

	let entries = $state<AuditEntry[]>([]);
	let cursor = $state<string | null>(null);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let people = $state<User[]>([]);
	let servers = $state<Instance[]>([]);
	let action = $state(any);
	let userId = $state(any);
	let instanceId = $state(any);

	// The action list is what the trail actually contains, not a hardcoded vocabulary: a new
	// audited action appears here the first time it is performed.
	const knownActions = $derived([...new Set(entries.map((e) => e.action))].sort());

	$effect(() => {
		void Promise.all([userAdmin.list(), instances.list()])
			.then(([u, i]) => {
				people = u;
				servers = i;
			})
			.catch(() => {
				// Filters degrade to "any" without their vocabulary; the trail itself still loads.
			});
	});

	// Re-runs whenever a filter changes, which is also what discards the old page's cursor.
	$effect(() => {
		void load({ action, user_id: userId, instance_id: instanceId });
	});

	function selected(value: string): string | undefined {
		return value === any ? undefined : value;
	}

	async function load(next: { action: string; user_id: string; instance_id: string }) {
		loading = true;
		failure = null;
		const filter: AuditFilter = {
			action: selected(next.action),
			user_id: selected(next.user_id),
			instance_id: selected(next.instance_id)
		};
		try {
			const page = await auditLog.list(filter);
			entries = page.items;
			cursor = page.next_cursor;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function loadMore() {
		if (!cursor) return;
		loading = true;
		try {
			const page = await auditLog.list(
				{
					action: selected(action),
					user_id: selected(userId),
					instance_id: selected(instanceId)
				},
				cursor
			);
			entries = [...entries, ...page.items];
			cursor = page.next_cursor;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function when(at: string): string {
		return new Date(at).toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
	}

	function actorOf(entry: AuditEntry): string {
		if (entry.actor) return entry.actor;
		return entry.user_id ? 'deleted user' : 'the panel';
	}
</script>

<main class="mx-auto grid max-w-5xl gap-6 p-6">
	<Button variant="ghost" size="sm" class="justify-self-start" href={resolve('/')}>
		<ArrowLeft /> Servers
	</Button>

	<header class="grid gap-1">
		<h1 class="text-2xl font-semibold tracking-tight">Audit log</h1>
		<p class="text-sm text-muted-foreground">
			The permanent record of privileged actions. It is never pruned, and it survives the users and
			servers it describes.
		</p>
	</header>

	<Problem error={failure} />

	<section class="grid gap-4 sm:grid-cols-3">
		<div class="grid gap-2">
			<Label for="filter-action">Action</Label>
			<Select.Root type="single" bind:value={action}>
				<Select.Trigger id="filter-action">{action === any ? 'Any action' : action}</Select.Trigger>
				<Select.Content>
					<Select.Item value={any}>Any action</Select.Item>
					{#each knownActions as name (name)}
						<Select.Item value={name}>{name}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
		<div class="grid gap-2">
			<Label for="filter-user">Actor</Label>
			<Select.Root type="single" bind:value={userId}>
				<Select.Trigger id="filter-user">
					{people.find((p) => p.id === userId)?.username ?? 'Anyone'}
				</Select.Trigger>
				<Select.Content>
					<Select.Item value={any}>Anyone</Select.Item>
					{#each people as person (person.id)}
						<Select.Item value={person.id}>{person.username}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
		<div class="grid gap-2">
			<Label for="filter-instance">Server</Label>
			<Select.Root type="single" bind:value={instanceId}>
				<Select.Trigger id="filter-instance">
					{servers.find((s) => s.id === instanceId)?.name ?? 'Any server'}
				</Select.Trigger>
				<Select.Content>
					<Select.Item value={any}>Any server</Select.Item>
					{#each servers as server (server.id)}
						<Select.Item value={server.id}>{server.name}</Select.Item>
					{/each}
				</Select.Content>
			</Select.Root>
		</div>
	</section>

	{#if entries.length === 0 && !loading}
		<div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
			No entries match these filters.
		</div>
	{/if}

	<div class="overflow-x-auto">
		<table class="w-full text-left text-sm">
			<thead class="text-xs text-muted-foreground">
				<tr>
					<th class="py-2 pr-4 font-medium">When</th>
					<th class="py-2 pr-4 font-medium">Actor</th>
					<th class="py-2 pr-4 font-medium">Action</th>
					<th class="py-2 pr-4 font-medium">Server</th>
					<th class="py-2 font-medium">Details</th>
				</tr>
			</thead>
			<tbody>
				{#each entries as entry (entry.id)}
					<tr class="border-t align-top">
						<td class="py-2 pr-4 whitespace-nowrap tabular-nums">{when(entry.created_at)}</td>
						<td class="py-2 pr-4">
							{actorOf(entry)}
							{#if entry.ip}
								<span class="block text-xs text-muted-foreground">{entry.ip}</span>
							{/if}
						</td>
						<td class="py-2 pr-4 font-mono text-xs">{entry.action}</td>
						<td class="py-2 pr-4">
							{entry.instance ?? (entry.instance_id ? 'deleted server' : '—')}
						</td>
						<td class="py-2">
							{#if entry.detail}
								<code class="text-xs break-all">{entry.detail}</code>
							{:else}
								—
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else if cursor}
		<Button variant="outline" class="justify-self-start" onclick={loadMore}>Load older</Button>
	{/if}
</main>
