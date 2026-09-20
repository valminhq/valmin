<script lang="ts">
	import { untrack } from 'svelte';
	import { ConsoleBuffer } from '$lib/state/console.svelte';
	import { VirtualList } from '$lib/virtual.svelte';
	import { Button } from '$lib/components/ui/button';
	import Problem from '$lib/components/problem.svelte';
	import ArrowUpToLine from '@lucide/svelte/icons/arrow-up-to-line';
	import ArrowDownToLine from '@lucide/svelte/icons/arrow-down-to-line';

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

	function time(ts: string): string {
		const d = new Date(ts);
		return Number.isNaN(d.valueOf()) ? '' : d.toLocaleTimeString();
	}
</script>

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
							class="absolute inset-x-0 flex h-5 items-center leading-5"
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
									<span class={row.stream === 'stderr' ? 'text-destructive' : ''}>{row.text}</span>
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
