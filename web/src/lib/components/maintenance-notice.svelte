<script lang="ts">
	import { schedules, type Schedule } from '$lib/api/schedules';
	import { socket } from '$lib/socket/index.svelte';
	import { topics } from '$lib/socket/messages';
	import * as Alert from '$lib/components/ui/alert';
	import Clock from '@lucide/svelte/icons/clock';

	let { instanceId }: { instanceId: string } = $props();

	let held = $state<Schedule[]>([]);

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
		const id = instanceId;
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

	const title = (s: Schedule) =>
		s.kind === 'backup'
			? 'Scheduled backup is waiting for players to leave'
			: 'Scheduled restart is waiting for players to leave';

	const latest = (s: Schedule) =>
		s.deferred_until
			? new Date(s.deferred_until).toLocaleString(undefined, { timeZone: s.timezone })
			: 'unknown';
</script>

{#each held as s (s.id)}
	<Alert.Root>
		<Clock />
		<Alert.Title>{title(s)}</Alert.Title>
		<Alert.Description>
			It runs as soon as no players are connected, or at {latest(s)}
			({s.timezone}) at the latest.
		</Alert.Description>
	</Alert.Root>
{/each}
