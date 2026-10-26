<script lang="ts">
	import { inZone, schedules, viewerZone, type Schedule } from '#lib/api/schedules.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { topics } from '#lib/socket/messages.js';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import Clock from '@lucide/svelte/icons/clock';

	let { instanceId }: { instanceId: string } = $props();

	let held = $state<Schedule[]>([]);
	const viewer = $derived(viewerZone());
	// Derived so a replaced row with the same id does not refetch.
	const id = $derived(instanceId);

	/** Reads the schedules of this instance that are holding a due run. A failed read keeps the
	 * last answer; the schedules editor is where a failure is reported. */
	async function load(id: string) {
		try {
			const page = await schedules.list();
			held = page.items.filter((s) => s.instance_id === id && s.deferred_since);
		} catch {
			// Retried on the next signal or reconnect.
		}
	}

	$effect(() => {
		const off = socket.subscribe(topics.state(id), (m) => {
			if (m.type === 'maintenance') void load(id);
		});
		const unhook = socket.onConnected(() => void load(id));
		void load(id);
		return () => {
			off();
			unhook();
		};
	});

	const titles: Record<string, string> = {
		backup: 'Scheduled backup is waiting for players to leave',
		chain: 'Scheduled maintenance is waiting for players to leave'
	};
	const title = (s: Schedule) =>
		titles[s.kind] ?? 'Scheduled restart is waiting for players to leave';

	const latest = (s: Schedule) => (s.deferred_until ? inZone(s.deferred_until, viewer) : 'unknown');
</script>

{#each held as s (s.id)}
	<Alert.Root>
		<Clock />
		<Alert.Title>{title(s)}</Alert.Title>
		<Alert.Description>
			It runs as soon as no players are connected, or at {latest(s)}
			({viewer}) at the latest.
		</Alert.Description>
	</Alert.Root>
{/each}
