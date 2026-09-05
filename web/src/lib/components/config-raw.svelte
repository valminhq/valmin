<script lang="ts">
	import { configs } from '$lib/api/configs';
	import { ApiError } from '$lib/api/errors';
	import type { TextResource } from '$lib/api/client';
	import { Button } from '$lib/components/ui/button';
	import * as Alert from '$lib/components/ui/alert';
	import Problem from '$lib/components/problem.svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	/**
	 * The file as text, for what the form cannot express.
	 *
	 * The ETag read with the bytes is held and sent back as `If-Match`, and it is the only
	 * way out of here (`11 §1.1`). This route replaces the whole file, so without it a
	 * second editor's save would take the first's with it and report success.
	 */
	let {
		id,
		file,
		editable,
		onsaved
	}: {
		id: string;
		file: string;
		editable: boolean;
		/** Called after a save lands, so the typed form re-reads rather than keeping the
		 * schema it parsed from bytes that are now gone (F4). */
		onsaved: () => void;
	} = $props();

	let text = $state('');
	let saved = $state('');
	let etag = $state('');
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);

	/** What is on disk now, fetched after a refused save. It is offered, never merged and
	 * never written on its own: which of the two versions survives is the operator's call. */
	let conflict = $state<TextResource | null>(null);

	const changed = $derived(text !== saved);

	$effect(() => {
		void load();
	});

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

	/** Save over the other version, having been shown it. A second deliberate write, on the
	 * ETag the conflict itself supplied — not a retry of the one that was refused. */
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

	{#if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else}
		<!--
			No validation and no widgets: this is the way out when the form cannot say what the
			file needs. The daemon still refuses the write on a running server, and still keeps
			the previous bytes beside the file.
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
