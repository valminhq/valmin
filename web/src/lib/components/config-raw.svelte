<script lang="ts">
	import { configs, type ConfigCopyName } from '$lib/api/configs';
	import { ApiError } from '$lib/api/errors';
	import type { TextResource } from '$lib/api/client';
	import { diffLines, hunks } from '$lib/diff';
	import { Button } from '$lib/components/ui/button';
	import * as Alert from '$lib/components/ui/alert';
	import Problem from '$lib/components/problem.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	/**
	 * The file as text, for what the form cannot express. The ETag read with the bytes is held
	 * and sent back as `If-Match`, without which this full replacement would silently overwrite
	 * another editor's save (`11 §1.1`).
	 */
	let {
		id,
		file,
		editable,
		compare,
		onsaved
	}: {
		id: string;
		file: string;
		editable: boolean;
		/** Which kept version to diff against, chosen once for the whole screen so this view and
		 * the form compare against the same one. */
		compare: ConfigCopyName | 'off';
		/** Called after a save lands, so the typed form re-reads rather than keeping a schema
		 * parsed from bytes that are gone (F4). */
		onsaved: () => void;
	} = $props();

	let text = $state('');
	let saved = $state('');
	let etag = $state('');
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	/** What is on disk now, fetched after a refused save. Offered, never merged or written on its
	 * own: which version survives is the operator's call. */
	let conflict = $state<TextResource | null>(null);

	const changed = $derived(text !== saved);

	/** The kept version's own bytes, or null when nothing is being compared. */
	let reference = $state<string | null>(null);

	/** Against the editor rather than the file, so an unsaved edit shows in the diff as what it
	 * will be. Recomputed per keystroke. */
	const groups = $derived(reference === null ? [] : hunks(diffLines(reference, text)));
	const changedLines = $derived(
		groups.reduce((n, group) => n + group.filter((line) => line.kind !== 'same').length, 0)
	);

	$effect(() => {
		void load();
	});

	$effect(() => {
		void loadReference(compare);
	});

	async function loadReference(which: ConfigCopyName | 'off') {
		if (which === 'off') {
			reference = null;
			return;
		}
		const read = await configs.readRawCopy(id, file, which).catch(() => null);
		reference = read?.text ?? null;
	}

	function take(read: TextResource) {
		text = read.text;
		saved = read.text;
		etag = read.etag;
		conflict = null;
	}

	async function load() {
		loading = true;
		try {
			take(await configs.readRaw(id, file));
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function save() {
		saving = true;
		failure = null;
		conflict = null;
		try {
			take(await configs.writeRaw(id, file, text, etag));
			onsaved();
		} catch (err) {
			failure = err;
			if (err instanceof ApiError && err.code === 'stale_write') {
				conflict = await configs.readRaw(id, file).catch(() => null);
			}
		} finally {
			saving = false;
		}
	}

	function discard() {
		text = saved;
		failure = null;
	}

	/** Drop this edit and continue from the version that was already on disk. */
	function theirs() {
		if (conflict) take(conflict);
		failure = null;
	}

	/** Save over the other version, having been shown it. A second deliberate write, on the ETag
	 * the conflict supplied. */
	async function mine() {
		if (!conflict) return;
		etag = conflict.etag;
		conflict = null;
		await save();
	}
</script>

<div class="grid gap-3">
	<Problem error={failure} />

	{#if conflict}
		<Alert.Root variant="destructive">
			<TriangleAlert />
			<Alert.Title>Nothing was saved</Alert.Title>
			<Alert.Description class="grid gap-3">
				<p>
					The file changed after you opened it. Here is what it holds now — keeping one version
					means losing the other, so neither is chosen for you.
				</p>
				<textarea
					class="h-48 w-full rounded-md border bg-background p-3 font-mono text-xs text-foreground"
					readonly
					aria-label="The file as it is on disk now">{conflict.text}</textarea
				>
				<div class="flex flex-wrap gap-2">
					<Button variant="outline" size="sm" onclick={theirs}>Use this and lose my edit</Button>
					<Button variant="outline" size="sm" onclick={mine}>Save mine over it</Button>
				</div>
			</Alert.Description>
		</Alert.Root>
	{/if}

	{#if reference !== null && !loading}
		<!--
			Line by line, unlike the form's by-setting comparison: a changed comment, a reordered
			section or a line the schema never modelled shows up only here.
		-->
		<div class="grid gap-3 rounded-md border p-4">
			<div class="flex flex-wrap items-center justify-between gap-2">
				<p class="text-sm text-muted-foreground">
					{changedLines === 0
						? 'Identical to the version being compared.'
						: `${changedLines} ${changedLines === 1 ? 'line differs' : 'lines differ'} from the version being compared.`}
				</p>
				{#if groups.length > 0}
					<Button
						variant="outline"
						size="sm"
						disabled={!editable}
						onclick={() => (text = reference ?? text)}
					>
						Load that version
					</Button>
				{/if}
			</div>

			{#if groups.length > 0}
				<div class="max-h-96 overflow-auto rounded-md border font-mono text-xs">
					{#each groups as group, i (i)}
						{#if i > 0}
							<div class="border-y bg-muted px-3 py-1 text-muted-foreground">⋯</div>
						{/if}
						{#each group as line (`${line.before}:${line.after}`)}
							<div
								class="flex gap-3 px-3 py-0.5 {line.kind === 'added'
									? 'bg-primary/10'
									: line.kind === 'removed'
										? 'bg-destructive/10'
										: ''}"
							>
								<span
									class="w-10 shrink-0 text-right text-muted-foreground tabular-nums select-none"
								>
									{line.before || ''}
								</span>
								<span
									class="w-10 shrink-0 text-right text-muted-foreground tabular-nums select-none"
								>
									{line.after || ''}
								</span>
								<span class="w-3 shrink-0 select-none">
									{line.kind === 'added' ? '+' : line.kind === 'removed' ? '−' : ''}
								</span>
								<span class="break-all whitespace-pre-wrap">{line.text}</span>
							</div>
						{/each}
					{/each}
				</div>
			{/if}
		</div>
	{/if}

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else}
		<!--
			No validation and no widgets. The daemon still refuses the write on a running server and
			still keeps the previous bytes beside the file.
		-->
		<textarea
			class="h-[32rem] w-full resize-y rounded-md border bg-background p-3 font-mono text-xs whitespace-pre disabled:opacity-50"
			spellcheck="false"
			autocapitalize="off"
			aria-label="{file} as text"
			disabled={!editable}
			bind:value={text}></textarea>

		<div class="flex flex-wrap items-center justify-between gap-3">
			<p class="text-sm text-muted-foreground">
				{changed ? 'Unsaved changes.' : 'Matches the file on disk.'}
			</p>
			<div class="flex gap-2">
				<Button variant="ghost" size="sm" onclick={discard} disabled={!changed || saving}>
					Discard
				</Button>
				<Button size="sm" onclick={save} disabled={!editable || !changed || !etag || saving}>
					{saving ? 'Saving…' : 'Save file'}
				</Button>
			</div>
		</div>
	{/if}
</div>
