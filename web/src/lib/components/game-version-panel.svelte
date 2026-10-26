<script lang="ts">
	import { resolve } from '$app/paths';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { actions, instances, type Instance, type SteamBranch } from '#lib/api/instances.js';
	import { session } from '#lib/state/session.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import GameBranchPicker from '#lib/components/game-branch-picker.svelte';
	import Problem from '#lib/components/problem.svelte';

	/** Chooses the Steam branch the next game update installs. Saving changes no files: the
	 * update on the overview does, with an archive taken first. */
	let { instance, onchange }: { instance: Instance; onchange?: () => void } = $props();

	let branches = $state<SteamBranch[] | null>(null);
	let branch = $state('public');
	let password = $state('');
	let saving = $state(false);
	/** The branch last saved here, so the way to apply it is offered until the form changes. */
	let saved = $state<string | null>(null);
	let failure = $state<unknown>(null);

	const canEdit = $derived(session.allowed(instance.id).includes(actions.gameUpdate));

	$effect(() => {
		branch = instance.game_branch;
		password = '';
	});

	// Only someone who can change the branch sees the picker, so no one else asks for its options.
	$effect(() => {
		if (!canEdit) return;
		instances.options().then(
			(options) => (branches = options.branches),
			(err) => (failure = err)
		);
	});

	const changed = $derived(branch.trim() !== instance.game_branch || password !== '');
	unsaved(() => changed);

	async function save() {
		saving = true;
		failure = null;
		try {
			await instances.patch(instance.id, {
				game_branch: branch.trim(),
				...(password ? { game_branch_password: password } : {})
			});
			saved = branch.trim();
			password = '';
			onchange?.();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
</script>

{#if canEdit}
	<Card.Root>
		<Card.Header>
			<Card.Title>Game version</Card.Title>
			<Card.Description>
				The Steam branch this server installs: the current release, an older one, or a test branch.
				The next game update switches to it.
			</Card.Description>
		</Card.Header>
		<Card.Content class="grid gap-4">
			<Problem error={failure} />
			{#if branches}
				{#key instance.game_branch}
					<GameBranchPicker {branches} bind:branch bind:password id="game-version" />
				{/key}
				<p class="text-sm text-muted-foreground">
					An older build may not load a world a newer one saved. The update archives the world
					first, so it can be restored.
				</p>
				<div class="flex flex-wrap items-center gap-3">
					<Button size="sm" disabled={!changed || branch.trim() === '' || saving} onclick={save}>
						{saving ? 'Saving…' : 'Save game version'}
					</Button>
					{#if saved !== null && saved === branch.trim() && password === ''}
						<a
							class="text-sm underline"
							href={resolve('/instances/[id]#update', { id: instance.id })}
							>Run the game update to switch</a
						>
					{/if}
				</div>
			{:else}
				<p class="text-sm text-muted-foreground">Loading…</p>
			{/if}
		</Card.Content>
	</Card.Root>
{/if}
