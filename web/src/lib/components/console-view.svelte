<script lang="ts">
	import { formatInstant } from '$lib/api/schedules';
	import { untrack } from 'svelte';
	import { ConsoleBuffer } from '$lib/state/console.svelte';
	import { VirtualList } from '$lib/virtual.svelte';
	import { findMatches, splitMatches } from '$lib/console-search';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import CopyButton from '$lib/components/copy-button.svelte';
	import Problem from '$lib/components/problem.svelte';
	import ArrowUpToLine from '@lucide/svelte/icons/arrow-up-to-line';
	import ArrowDownToLine from '@lucide/svelte/icons/arrow-down-to-line';
	import ChevronUp from '@lucide/svelte/icons/chevron-up';
	import ChevronDown from '@lucide/svelte/icons/chevron-down';

	let {
		buffer,
		commandChannel,
		canSend,
		running
	}: {
		buffer: ConsoleBuffer;
		commandChannel: 'rcon' | 'stdin' | 'none';
		canSend: boolean;
		running: boolean;
	} = $props();

	let command = $state('');
	let sending = $state(false);
	let commandError = $state<unknown>(null);
	const commandEnabled = $derived(commandChannel !== 'none' && canSend && running && !sending);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const value = command.trim();
		if (!value || !commandEnabled) return;
		sending = true;
		commandError = null;
		try {
			await buffer.command(value);
			command = '';
		} catch (error) {
			commandError = error;
		} finally {
			sending = false;
		}
	}

	const commandReason = $derived.by(() => {
		if (commandChannel === 'none') return 'Install Tristan-ValheimRcon to send commands.';
		if (!canSend) return 'You do not have permission to send commands.';
		if (!running) return 'Start the server before sending a command.';
		return 'Commands are sent through the server’s private RCON channel.';
	});

	/** Every row is exactly one line: `white-space: pre` with a horizontal scroller, which is
	 * what a console does anyway. Fixed heights mean the virtualizer never has to measure a
	 * rendered row, and the scrollbar never resizes under the pointer. */
	const ROW_HEIGHT = 20;

	let scroller = $state<HTMLElement | null>(null);
	let list = $state<VirtualList | null>(null);
	let following = $state(true);

	$effect(() => {
		if (!scroller) return;
		const l = new VirtualList(
			scroller,
			untrack(() => buffer.rows.length),
			ROW_HEIGHT
		);
		const stop = l.mount();
		list = l;
		l.scrollToEnd();
		return () => {
			stop();
			list = null;
		};
	});

	$effect(() => {
		const count = buffer.rows.length;
		list?.setCount(count);
		if (following) list?.scrollToEnd();
	});

	function onscroll() {
		following = list?.isAtEnd(48) ?? true;
	}

	function toStart() {
		following = false;
		list?.scrollToIndex(0, 'start');
	}

	function toEnd() {
		following = true;
		list?.scrollToEnd();
	}

	let query = $state('');
	let active = $state(0);

	/** The rows whose text contains the query. Breaks are annotations, not log lines, so they
	 * never match. */
	const matches = $derived(
		query === ''
			? []
			: findMatches(
					buffer.rows.map((row) => (row.kind === 'line' ? row.text : '')),
					query
				)
	);
	/** The active match's position, held inside the list when matches disappear under it. */
	const position = $derived(Math.min(active, Math.max(matches.length - 1, 0)));
	const activeRow = $derived(matches[position]);
	const counter = $derived.by(() => {
		if (query === '') return '';
		return matches.length === 0 ? 'No matches' : `${position + 1} of ${matches.length}`;
	});

	/** Brings the active match into view, leaving the scroll alone when it is already visible,
	 * and pauses auto-scroll so arriving lines do not carry it away. */
	function reveal() {
		const row = matches[position];
		if (row === undefined || !scroller) return;
		following = false;
		const top = row * ROW_HEIGHT;
		if (top < scroller.scrollTop || top + ROW_HEIGHT > scroller.scrollTop + scroller.clientHeight) {
			list?.scrollToIndex(row, 'center');
		}
	}

	/** Sets the query and moves to its first match. */
	function search(value: string) {
		query = value;
		active = 0;
		reveal();
	}

	/** Moves to the next or previous match, wrapping around at either end. */
	function step(delta: 1 | -1) {
		if (matches.length === 0) return;
		active = (position + delta + matches.length) % matches.length;
		reveal();
	}

	/** Enter and Shift+Enter step through the matches; Escape clears the search. */
	function onSearchKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter') {
			event.preventDefault();
			step(event.shiftKey ? -1 : 1);
		} else if (event.key === 'Escape' && query !== '') {
			event.preventDefault();
			search('');
		}
	}

	let selected = $state('');

	/** Tracks the text selected inside the log; a selection reaching outside it does not count. */
	function readSelection() {
		const selection = document.getSelection();
		const inside =
			selection &&
			!selection.isCollapsed &&
			scroller?.contains(selection.getRangeAt(0).commonAncestorContainer);
		selected = inside ? selection.toString() : '';
	}

	function time(ts: string): string {
		const d = new Date(ts);
		return Number.isNaN(d.valueOf()) ? '' : formatInstant(d, { timeStyle: 'medium' });
	}
</script>

<svelte:document onselectionchange={readSelection} />

<!-- One line of text with its search hits marked; the hits of the active row stand out most. -->
{#snippet highlighted(text: string, current: boolean)}
	{#each splitMatches(text, query) as part, index (index)}
		{#if part.match}
			<mark
				class="rounded-sm {current
					? 'bg-amber-500 text-amber-950 dark:bg-amber-400'
					: 'bg-amber-200 text-amber-950 dark:bg-amber-900/60 dark:text-amber-100'}"
				>{part.text}</mark
			>
		{:else}
			{part.text}
		{/if}
	{/each}
{/snippet}

<div class="grid gap-2">
	<div class="flex items-center gap-2">
		<!--
			G8. The pinned startup segment is the first thing the server's ring drops and
			the only thing that explains a boot that failed, so it gets a control of its own
			rather than being something an operator has to scroll for and hope survived.
		-->
		<Button variant="outline" size="sm" onclick={toStart} disabled={buffer.rows.length === 0}>
			<ArrowUpToLine />
			First log entry
		</Button>
		<Button variant="outline" size="sm" onclick={toEnd} disabled={following}>
			<ArrowDownToLine /> Follow latest
		</Button>
		{#if !following}
			<span class="text-xs text-muted-foreground">Auto-scroll paused</span>
		{/if}
	</div>

	<div class="flex flex-wrap items-center gap-2">
		<label class="sr-only" for="console-search">Search the console</label>
		<Input
			id="console-search"
			type="search"
			value={query}
			oninput={(event) => search(event.currentTarget.value)}
			onkeydown={onSearchKeydown}
			autocomplete="off"
			placeholder="Search the log"
			class="h-7 max-w-xs min-w-40 flex-1 text-xs md:text-xs"
		/>
		<Button
			variant="outline"
			size="icon-sm"
			onclick={() => step(-1)}
			disabled={matches.length === 0}
			aria-label="Previous match"
		>
			<ChevronUp />
		</Button>
		<Button
			variant="outline"
			size="icon-sm"
			onclick={() => step(1)}
			disabled={matches.length === 0}
			aria-label="Next match"
		>
			<ChevronDown />
		</Button>
		<span aria-live="polite" class="min-w-16 text-xs text-muted-foreground tabular-nums">
			{counter}
		</span>
		<!-- Disabled while nothing in the log is selected. -->
		<fieldset disabled={selected === ''}>
			<CopyButton value={selected} label="Copy selected text" size="sm" />
		</fieldset>
	</div>

	{#if buffer.error}
		<p class="rounded-md border p-3 text-sm text-muted-foreground">
			The console could not be loaded. Check your connection and access to this server.
		</p>
	{:else}
		<div
			bind:this={scroller}
			{onscroll}
			class="h-[28rem] overflow-auto rounded-md border bg-muted/30 font-mono text-xs"
			role="log"
			aria-label="Server console"
		>
			<div class="relative w-max min-w-full" style="height: {list?.total ?? 0}px">
				{#each list?.items ?? [] as item (item.key)}
					{@const row = buffer.rows[item.index]}
					{#if row}
						<div
							class="absolute inset-x-0 flex h-5 items-center leading-5 {item.index === activeRow
								? 'bg-accent'
								: ''}"
							aria-current={item.index === activeRow ? 'true' : undefined}
							style="transform: translateY({item.start}px)"
						>
							{#if row.kind === 'break'}
								<!--
									A visible break, never a seam (ADR-039, `14 §4.2`). Lines are
									missing here — because this browser fell behind, or because the
									server's ring rotated past them — and closing the hole silently
									would let a reader draw a conclusion from adjacency that is not
									there.
								-->
								<span
									class="flex w-full items-center gap-2 px-2 text-muted-foreground italic
										before:h-px before:flex-1 before:bg-border
										after:h-px after:flex-1 after:bg-border"
								>
									{row.text}
								</span>
							{:else}
								<span class="px-2 whitespace-pre">
									<span class="text-muted-foreground">{time(row.ts)}</span>
									<span class={row.stream === 'stderr' ? 'text-destructive' : ''}
										>{@render highlighted(row.text, item.index === activeRow)}</span
									>
								</span>
							{/if}
						</div>
					{/if}
				{/each}
			</div>
		</div>
	{/if}

	<div class="grid gap-1">
		<Problem error={commandError} />
		<form class="flex gap-2" onsubmit={submit}>
			<label class="sr-only" for="console-command">Server command</label>
			<input
				id="console-command"
				type="text"
				bind:value={command}
				disabled={!commandEnabled}
				maxlength="1024"
				autocomplete="off"
				placeholder={commandChannel === 'none' ? 'Commands are not available' : 'Enter a command'}
				aria-describedby="console-input-reason"
				class="min-w-0 flex-1 rounded-md border bg-background px-3 py-2 font-mono text-xs
					disabled:cursor-not-allowed disabled:bg-muted/30 disabled:text-muted-foreground"
			/>
			<Button type="submit" size="sm" disabled={!commandEnabled || command.trim() === ''}>
				{sending ? 'Sending…' : 'Send'}
			</Button>
		</form>
		<p id="console-input-reason" class="text-sm text-muted-foreground">
			{commandReason}
		</p>
	</div>
</div>
