<script lang="ts">
	import { ApiError } from '$lib/api/errors';
	import { playerLists, type PlayerList, type PlayerListKind } from '$lib/api/players';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Alert from '$lib/components/ui/alert';
	import { unsaved } from '$lib/state/dirty.svelte';
	import Problem from '$lib/components/problem.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let {
		instanceId,
		kind,
		title,
		description
	}: {
		instanceId: string;
		kind: PlayerListKind;
		title: string;
		description: string;
	} = $props();

	let text = $state('');
	let baseline = $state('');
	let etag = $state('');
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let conflict = $state<{ data: PlayerList; etag: string } | null>(null);

	const changed = $derived(text !== baseline);
	unsaved(() => changed);
	const apiError = $derived(failure instanceof ApiError ? failure : null);
	const fieldErrors = $derived(
		(apiError?.fields ?? []).filter((field) => field.field.startsWith('ids.'))
	);

	$effect(() => {
		void load();
	});

	function asText(ids: string[]): string {
		return ids.join('\n');
	}

	function asIDs(value: string): string[] {
		return value.split('\n');
	}

	function adopt(data: PlayerList, nextETag: string) {
		text = asText(data.ids);
		baseline = text;
		etag = nextETag;
		conflict = null;
		failure = null;
	}

	async function load() {
		loading = true;
		try {
			const loaded = await playerLists.get(instanceId, kind);
			adopt(loaded.data, loaded.etag);
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function save(match = etag) {
		saving = true;
		failure = null;
		try {
			const saved = await playerLists.put(instanceId, kind, asIDs(text), match);
			adopt(saved.data, saved.etag);
		} catch (err) {
			if (err instanceof ApiError && err.code === 'stale_write') {
				try {
					conflict = await playerLists.get(instanceId, kind);
				} catch (reloadError) {
					failure = reloadError;
				}
			} else {
				failure = err;
			}
		} finally {
			saving = false;
		}
	}

	function useCurrent() {
		if (conflict) adopt(conflict.data, conflict.etag);
	}

	function overwriteCurrent() {
		if (conflict) void save(conflict.etag);
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>{title}</Card.Title>
		<Card.Description>{description}</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-3">
		{#if loading}
			<p class="text-sm text-muted-foreground">Loading…</p>
		{:else}
			<label class="grid gap-2">
				<span class="text-sm font-medium">Player IDs</span>
				<textarea
					bind:value={text}
					rows="7"
					spellcheck="false"
					class="min-h-32 w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 aria-invalid:border-destructive aria-invalid:ring-destructive/20"
					aria-invalid={fieldErrors.length > 0}
					aria-describedby={`${kind}-help ${kind}-errors`}></textarea>
			</label>
			<p id={`${kind}-help`} class="text-xs text-muted-foreground">
				One ID per line. Bare and platform-prefixed IDs are both kept as entered. Existing comments
				are retained but not shown here.
			</p>

			{#if fieldErrors.length > 0}
				<ul id={`${kind}-errors`} class="grid gap-1 text-sm text-destructive">
					{#each fieldErrors as issue (issue.field)}
						{@const line = Number(issue.field.slice('ids.'.length)) + 1}
						<li>Line {line}: {issue.message}</li>
					{/each}
				</ul>
			{:else}
				<Problem error={failure} />
			{/if}

			{#if conflict}
				<Alert.Root>
					<TriangleAlert />
					<Alert.Title>This list changed elsewhere</Alert.Title>
					<Alert.Description class="grid gap-3">
						<p>
							Your edits are still in the field above. Discard them to use the saved version below,
							or overwrite the saved list with your edits:
						</p>
						<pre
							class="max-h-32 overflow-auto rounded-md bg-muted p-3 text-xs whitespace-pre-wrap">{asText(
								conflict.data.ids
							) || '(empty list)'}</pre>
						<div class="flex flex-wrap gap-2">
							<Button variant="outline" size="sm" onclick={useCurrent}>Discard my edits</Button>
							<Button size="sm" disabled={saving} onclick={overwriteCurrent}>
								{saving ? 'Saving…' : 'Overwrite saved list'}
							</Button>
						</div>
					</Alert.Description>
				</Alert.Root>
			{/if}

			<div class="flex items-center justify-end gap-2">
				<Button
					variant="ghost"
					size="sm"
					disabled={!changed || saving}
					onclick={() => (text = baseline)}
				>
					Discard changes
				</Button>
				<Button
					size="sm"
					disabled={!changed || saving || conflict !== null}
					onclick={() => void save()}
				>
					{saving ? 'Saving…' : 'Save list'}
				</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>
