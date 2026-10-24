<script lang="ts">
	import {
		mods,
		sourceBadge,
		sourceLabel,
		type CodePreview,
		type CodeSkip,
		type ModConflict,
		type UpdateTarget
	} from '$lib/api/mods';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import { Label } from '$lib/components/ui/label';
	import * as Alert from '$lib/components/ui/alert';
	import * as Dialog from '$lib/components/ui/dialog';
	import Problem from '$lib/components/problem.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let {
		id,
		open = $bindable(false),
		blocked,
		onapply
	}: {
		id: string;
		open: boolean;
		/** Why nothing can be applied right now, or null. */
		blocked: string | null;
		onapply: (targets: UpdateTarget[], registry: CodePreview['registry']) => void;
	} = $props();

	const skipText: Record<CodeSkip, string> = {
		locked: 'locked',
		disabled: 'disabled on this server',
		modpack: 'managed by a modpack',
		framework: 'the mod framework is managed by the panel',
		other_registry: 'installed from another registry',
		not_in_registry: 'this version is not in the registry it is installed from',
		registry_disabled: 'its registry is not enabled',
		unknown_registry: 'the code names a registry this panel does not know',
		unavailable: 'not in any enabled registry'
	};

	let code = $state('');
	/** The code the preview was built from, which a re-check uses even when the box has since changed. */
	let checkedCode = '';
	let preview = $state<CodePreview | null>(null);
	let failure = $state<unknown>(null);
	let checking = $state(false);
	/** Counts checks, so an answer that arrives after a newer check or after closing is dropped. */
	let latest = 0;
	/** Changes after every check, so the checkboxes follow the preview again if a re-check failed. */
	let listKey = $state(0);

	const actionable = $derived(preview?.rows.filter((r) => r.change && !r.skip) ?? []);
	const skipped = $derived(preview?.rows.filter((r) => r.skip) ?? []);

	$effect(() => {
		if (!open) {
			latest++;
			code = '';
			checkedCode = '';
			preview = null;
			failure = null;
			checking = false;
		}
	});

	/** Asks the daemon what the code would do; `select` re-asks after a row is ticked, since the
	 * dependencies the change pulls in depend on the choice. */
	async function check(select?: string[]) {
		const mine = ++latest;
		const asked = select === undefined ? code.trim() : checkedCode;
		checking = true;
		failure = null;
		try {
			const answer = await mods.previewCode(id, asked, select);
			if (mine !== latest) return;
			preview = answer;
			checkedCode = asked;
		} catch (err) {
			if (mine !== latest) return;
			failure = err;
			if (select === undefined) preview = null;
		} finally {
			if (mine === latest) {
				checking = false;
				listKey++;
			}
		}
	}

	function toggle(fullName: string, on: boolean) {
		if (!preview) return;
		const chosen = preview.rows
			.filter((r) => r.selected && r.full_name !== fullName)
			.map((r) => r.full_name);
		void check(on ? [...chosen, fullName] : chosen);
	}

	function conflictText(conflict: ModConflict): string {
		const left = conflict.have ? `leaves it at ${conflict.have}` : 'removes it';
		const lock = conflict.locked ? ` ${conflict.dependency} is locked.` : '';
		return `${conflict.full_name} ${conflict.version} needs ${conflict.dependency} ${conflict.requires} or newer, and this change ${left}.${lock}`;
	}

	const changeLabel = { install: 'new', upgrade: 'update', downgrade: 'downgrade' } as const;

	function apply() {
		if (!preview) return;
		open = false;
		onapply(preview.targets, preview.registry);
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Content class="sm:max-w-2xl">
		<Dialog.Header>
			<Dialog.Title>Update mods from a code</Dialog.Title>
			<Dialog.Description>
				Paste a profile code from r2modman or Gale. Mods on this server that are not in the code
				stay as they are, and a mod stays in the registry it is installed from.
			</Dialog.Description>
		</Dialog.Header>

		<div class="grid gap-2">
			<Label for="profile-code">Profile code</Label>
			<div class="flex gap-2">
				<input
					id="profile-code"
					spellcheck="false"
					autocomplete="off"
					placeholder="01234567-89ab-cdef-0123-456789abcdef"
					class="w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
					bind:value={code}
				/>
				<Button
					variant="outline"
					disabled={code.trim() === '' || checking}
					onclick={() => void check()}
				>
					{checking ? 'Checking…' : 'Check code'}
				</Button>
			</div>
		</div>
		<Problem error={failure} />

		{#if preview}
			<p class="text-sm text-muted-foreground">
				{preview.profile_name || 'This profile'}: {preview.matching}
				{preview.matching === 1 ? 'mod already matches' : 'mods already match'}.
			</p>
			{#if actionable.length === 0}
				<p class="text-sm">Nothing in this code would change this server.</p>
			{:else}
				<ul class="grid max-h-56 gap-2 overflow-y-auto text-sm" data-testid="code-rows">
					{#key listKey}
						{#each actionable as row (row.full_name)}
							<li class="flex flex-wrap items-center gap-2">
								<input
									id="code-{row.full_name}"
									type="checkbox"
									class="size-4 accent-primary"
									checked={row.selected}
									disabled={checking}
									onchange={(e) => toggle(row.full_name, e.currentTarget.checked)}
								/>
								<Label for="code-{row.full_name}" class="font-medium">{row.full_name}</Label>
								<span class="text-muted-foreground tabular-nums">
									{row.installed_version ? `${row.installed_version} → ` : ''}{row.version}
								</span>
								<Badge variant="outline">{row.change ? changeLabel[row.change] : ''}</Badge>
								{#if row.source}
									<Badge variant="outline" class={sourceBadge[row.source]}>
										{sourceLabel[row.source]}
									</Badge>
								{/if}
							</li>
						{/each}
					{/key}
				</ul>
			{/if}
			{#if skipped.length > 0}
				<div class="grid gap-1 text-sm" data-testid="code-skipped">
					<p class="font-medium">Left alone</p>
					{#each skipped as row (row.full_name)}
						<p class="text-muted-foreground">
							{row.full_name}
							{row.installed_version ? `${row.installed_version} → ` : ''}{row.version}: {row.skip
								? skipText[row.skip]
								: ''}.
						</p>
					{/each}
				</div>
			{/if}
			{@const added = preview.nodes.filter((n) => n.transitive)}
			{#if added.length > 0}
				<p class="text-sm text-muted-foreground">
					Also changes {added.length === 1 ? 'a dependency' : `${added.length} dependencies`}:
					{added
						.map((n) => `${n.full_name} ${n.version} (${sourceLabel[n.source] ?? n.source})`)
						.join(', ')}.
				</p>
			{/if}
			{#if preview.problems.length > 0 || preview.conflicts.length > 0}
				<Alert.Root variant="destructive">
					<TriangleAlert />
					<Alert.Title>These changes cannot be applied as chosen</Alert.Title>
					<Alert.Description class="grid gap-1">
						{#each preview.problems as problem (problem)}
							<span>{problem}</span>
						{/each}
						{#each preview.conflicts as conflict (`${conflict.full_name}:${conflict.dependency}`)}
							<span>{conflictText(conflict)}</span>
						{/each}
					</Alert.Description>
				</Alert.Root>
			{/if}
			<p class="text-sm text-muted-foreground">
				{preview.backup
					? 'The world is backed up first. The backup is kept even if the change fails.'
					: 'This server has no world yet, so there is nothing to back up.'}
			</p>
			{#if blocked}
				<p class="text-sm text-muted-foreground" data-testid="code-blocked">{blocked}</p>
			{/if}
		{/if}

		<Dialog.Footer>
			<Button variant="outline" onclick={() => (open = false)}>Cancel</Button>
			<Button
				disabled={!preview ||
					preview.targets.length === 0 ||
					preview.problems.length > 0 ||
					preview.conflicts.length > 0 ||
					blocked !== null ||
					checking}
				onclick={apply}>Back up and apply</Button
			>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
