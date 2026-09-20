<script lang="ts">
	import type { Instance } from '$lib/api/instances';
	import { Button } from '$lib/components/ui/button';
	import Copy from '@lucide/svelte/icons/copy';
	import Check from '@lucide/svelte/icons/check';

	let { instance }: { instance: Instance } = $props();

	let copied = $state(false);
	let copyFailed = $state(false);
	let clear: ReturnType<typeof setTimeout> | undefined;

	/** The clipboard API is unavailable over plain HTTP, which is how a panel on a LAN address is
	 * usually reached, so the failure is reported rather than swallowed: the code stays selectable
	 * either way. */
	async function copyCode(code: string) {
		try {
			await navigator.clipboard.writeText(code);
		} catch {
			copyFailed = true;
			return;
		}
		copyFailed = false;
		copied = true;
		clearTimeout(clear);
		clear = setTimeout(() => (copied = false), 2000);
	}
</script>

<section class="grid gap-3 rounded-lg border bg-card p-4" aria-labelledby="connect-heading">
	<h2 id="connect-heading" class="text-sm font-medium">Connect to this server</h2>

	<dl class="grid gap-2 text-sm">
		{#if instance.crossplay}
			<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
				<dt class="text-muted-foreground">Join code</dt>
				<dd class="flex items-center gap-2">
					{#if instance.crossplay_join_code}
						<code
							class="rounded-md bg-muted px-2 py-0.5 font-mono text-sm font-semibold tracking-wider select-all"
							>{instance.crossplay_join_code}</code
						>
						<Button
							variant="ghost"
							size="sm"
							onclick={() => copyCode(instance.crossplay_join_code ?? '')}
						>
							{#if copied}<Check />Copied{:else}<Copy />Copy{/if}
						</Button>
					{:else}
						<!-- The code is latched out of the log seconds after the server reaches running, and
						     a restart invalidates the previous one (Q25), so its absence has two causes and
						     the operator is told which applies. -->
						<span class="text-muted-foreground">
							{instance.state === 'running'
								? 'Not logged yet — it appears within a few seconds of the server starting.'
								: 'Available once the server is running.'}
						</span>
					{/if}
				</dd>
			</div>
		{/if}

		<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
			<dt class="text-muted-foreground">Port</dt>
			<dd class="tabular-nums">UDP {instance.base_port}–{instance.base_port + 1}</dd>
		</div>

		{#if instance.public}
			<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
				<dt class="text-muted-foreground">Community browser</dt>
				<dd>Listed as <span class="font-medium">{instance.server_name}</span></dd>
			</div>
		{/if}

		<div class="flex flex-wrap items-center gap-x-3 gap-y-1">
			<dt class="text-muted-foreground">World</dt>
			<dd>{instance.world_name}</dd>
		</div>
	</dl>

	<!-- `02 §5`: a container's address is not a route to it. Valmin knows the port it published on
	     the host and nothing about how a player reaches that host, so it says so rather than
	     offering an address that may not resolve. -->
	<p class="text-sm text-muted-foreground">
		Players connect to the address of the host this server runs on, with the port above. Valmin does
		not know that address and cannot confirm it is reachable from outside.
	</p>

	{#if copyFailed}
		<p class="text-xs text-destructive">
			This browser will not copy over an insecure connection. Select the code and copy it by hand.
		</p>
	{/if}
</section>
