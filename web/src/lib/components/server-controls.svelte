<script lang="ts">
	import { actions, instances, type Instance } from '#lib/api/instances.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import Play from '@lucide/svelte/icons/play';
	import Square from '@lucide/svelte/icons/square';
	import RotateCw from '@lucide/svelte/icons/rotate-cw';

	let {
		instance,
		allowed,
		owes = false,
		onfailure
	}: {
		instance: Instance;
		allowed: string[];
		/** The server's setup has steps left to run, which holds Start. */
		owes?: boolean;
		/** Told each request's error, and null when a new request starts. */
		onfailure: (error: unknown) => void;
	} = $props();

	let busy = $state(false);
	let asking = $state<'stop' | 'restart'>('stop');
	let confirmOpen = $state(false);

	const canStart = $derived(instance.state === 'stopped' && allowed.includes(actions.start));
	const canStop = $derived(instance.state === 'running' && allowed.includes(actions.stop));
	const canRestart = $derived(instance.state === 'running' && allowed.includes(actions.restart));

	async function run(action: () => Promise<unknown>) {
		busy = true;
		onfailure(null);
		try {
			await action();
		} catch (err) {
			onfailure(err);
		} finally {
			busy = false;
		}
	}

	function ask(what: 'stop' | 'restart') {
		asking = what;
		confirmOpen = true;
	}

	function confirmed() {
		confirmOpen = false;
		const { id } = instance;
		void run(() => (asking === 'stop' ? instances.stop(id) : instances.restart(id)));
	}
</script>

<!--
	Start, Stop and Restart for one server: only the ones its state and the viewer's
	permissions allow, and nothing at all when there are none. Stop and Restart ask first,
	because both disconnect whoever is playing.
-->
{#if canStart || canStop || canRestart}
	<div
		role="group"
		aria-label="Controls for {instance.name}"
		class="flex flex-wrap items-center gap-2"
	>
		{#if canStart}
			<Button
				size="sm"
				disabled={busy || owes}
				onclick={() => run(() => instances.start(instance.id))}
			>
				<Play /> Start
			</Button>
		{/if}
		{#if canStop}
			<Button variant="outline" size="sm" disabled={busy} onclick={() => ask('stop')}>
				<Square /> Stop
			</Button>
		{/if}
		{#if canRestart}
			<Button variant="outline" size="sm" disabled={busy} onclick={() => ask('restart')}>
				<RotateCw /> Restart
			</Button>
		{/if}
	</div>
	<DestructiveConfirm
		bind:open={confirmOpen}
		title={asking === 'stop' ? `Stop ${instance.name}?` : `Restart ${instance.name}?`}
		description={asking === 'stop'
			? 'Connected players are disconnected while the server saves the world and stops.'
			: 'Connected players are disconnected while the server saves the world and restarts.'}
		confirmLabel={asking === 'stop' ? 'Stop' : 'Restart'}
		onconfirm={confirmed}
	/>
{/if}
