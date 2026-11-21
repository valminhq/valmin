<script lang="ts">
	import { Textarea } from '#lib/components/ui/textarea/index.js';
	import { ApiError } from '#lib/api/errors.js';
	import {
		playerLists,
		type PlayerList,
		type PlayerListKind,
		type PlayerName,
		type SeenPlayer
	} from '#lib/api/players.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import { untrack } from 'svelte';
	import { latestCall } from '#lib/latest.js';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import Problem from '#lib/components/problem.svelte';
	import SaveFooter from '#lib/components/save-footer.svelte';
	import PlayerNames from '#lib/components/player-names.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let {
		instanceId,
		kind,
		title,
		description,
		hintId,
		players = [],
		revision = 0
	}: {
		instanceId: string;
		kind: PlayerListKind;
		title: string;
		description: string;
		/** The id of the shared hint that describes the field. */
		hintId: string;
		/** The accounts the server has seen, for the names shown beside each id. */
		players?: SeenPlayer[];
		/** Bumped when the list was changed from elsewhere on the page; reloads a clean field. */
		revision?: number;
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
		void revision;
		if (untrack(() => !changed)) void load();
	});

	// Keyed by the full id and by the part after the platform prefix, so a bare SteamID64 line
	// finds the account the server printed as Steam_<id>.
	const namesByID = $derived.by(() => {
		const byID: Record<string, PlayerName[]> = Object.create(null);
		for (const p of players) {
			byID[p.platform_id] = p.names;
			byID[p.platform_id.slice(p.platform_id.indexOf('_') + 1)] ??= p.names;
		}
		return byID;
	});
	const entries = $derived(
		[...new Set(asIDs(text).map((line) => line.trim()))].filter((id) => id !== '')
	);

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

	const begin = latestCall();

	async function load() {
		loading = true;
		const current = begin();
		try {
			const loaded = await playerLists.get(instanceId, kind);
			if (current()) adopt(loaded.data, loaded.etag);
		} catch (err) {
			if (current()) failure = err;
		} finally {
			if (current()) loading = false;
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
				<Textarea
					bind:value={text}
					rows={7}
					spellcheck="false"
					class="min-h-32 font-mono"
					aria-invalid={fieldErrors.length > 0}
					aria-describedby={fieldErrors.length > 0 ? `${hintId} ${kind}-errors` : hintId}
				></Textarea>
			</label>

			{#if entries.length > 0 && players.length > 0}
				<ul class="grid max-h-40 gap-1 overflow-auto" aria-label="{title}: names">
					{#each entries as id (id)}
						{@const names = namesByID[id] ?? []}
						<li class="flex flex-wrap items-baseline gap-2">
							<span class="font-mono text-xs break-all">{id}</span>
							{#if names.length > 0}
								<PlayerNames {names} />
							{:else}
								<span class="text-xs text-muted-foreground">not seen on this server</span>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}

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

			<SaveFooter
				saveLabel="Save list"
				{changed}
				busy={saving}
				invalid={conflict !== null}
				ondiscard={() => {
					text = baseline;
					void load();
				}}
				onsave={() => void save()}
			/>
		{/if}
	</Card.Content>
</Card.Root>
