<script lang="ts">
	import type { SteamBranch } from '#lib/api/instances.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import * as Select from '#lib/components/ui/select/index.js';
	import { untrack } from 'svelte';

	/** Picks a branch from the ones Steam lists, or names a password branch, which Steam never
	 * lists. `password` is empty for a listed branch. */
	let {
		branches,
		branch = $bindable('public'),
		password = $bindable(''),
		id = 'game-branch'
	}: { branches: SteamBranch[]; branch?: string; password?: string; id?: string } = $props();

	/** Not a branch name: those never contain a space. */
	const PASSWORD_BRANCH = 'password branch';
	const listed = (name: string) => name === 'public' || branches.some((b) => b.name === name);
	let choice = $state(untrack(() => (listed(branch) ? branch : PASSWORD_BRANCH)));

	const label = (name: string) => {
		const description = branches.find((b) => b.name === name)?.description;
		return description ? `${name} — ${description}` : name;
	};

	function choose(value: string) {
		choice = value;
		branch = value === PASSWORD_BRANCH ? '' : value;
		password = '';
	}
</script>

<div class="grid gap-2">
	<Label for={id}>Game version</Label>
	<Select.Root type="single" value={choice} onValueChange={choose}>
		<Select.Trigger {id}>
			{choice === PASSWORD_BRANCH ? 'Password branch' : label(choice)}
		</Select.Trigger>
		<Select.Content>
			{#if !branches.some((b) => b.name === 'public')}
				<Select.Item value="public">public</Select.Item>
			{/if}
			{#each branches as b (b.name)}
				<Select.Item value={b.name}>{label(b.name)}</Select.Item>
			{/each}
			<Select.Item value={PASSWORD_BRANCH}>Password branch…</Select.Item>
		</Select.Content>
	</Select.Root>
	{#if choice === PASSWORD_BRANCH}
		<div class="grid gap-3 sm:grid-cols-2">
			<div class="grid gap-2">
				<Label for="{id}-name">Branch name</Label>
				<Input id="{id}-name" bind:value={branch} placeholder="public-test" autocomplete="off" />
			</div>
			<div class="grid gap-2">
				<Label for="{id}-password">Branch password</Label>
				<Input
					id="{id}-password"
					type="password"
					bind:value={password}
					autocomplete="off"
					required
				/>
			</div>
		</div>
		<p class="text-sm text-muted-foreground">
			The game's developers publish the name and password of a test branch with its announcement.
		</p>
	{/if}
	{#if choice !== 'public'}
		<p class="text-sm text-muted-foreground">
			Players need the same branch: in Steam, open Valheim's Properties, then Betas.
		</p>
	{/if}
</div>
