<script lang="ts">
	import { resolve } from '$app/paths';
	import type { InboxItem, InboxKind } from '$lib/api/inbox';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Alert from '$lib/components/ui/alert';
	import * as Card from '$lib/components/ui/card';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

	let { items }: { items: InboxItem[] } = $props();

	// What each condition means, in the operator's terms. The daemon sends facts, never copy.
	const sentence: Record<InboxKind, string> = {
		job_failed: 'A job failed and nothing has retried it',
		low_disk: 'The host is running out of disk space',
		stale_backup: 'Scheduled backups are not producing archives',
		unclean_stop: 'A server stopped before its world finished saving',
		restart_required: 'A change is waiting for a restart',
		update_available: 'A game update is available',
		instance_error: 'A server is in an error state',
		crash_loop: 'A server is crashing repeatedly',
		job_stuck: 'A job has been running unusually long'
	};

	const critical = $derived(items.filter((item) => item.severity === 'critical').length);

	function since(item: InboxItem): string {
		const started = new Date(item.since_at).getTime();
		const minutes = Math.max(0, Math.round((Date.now() - started) / 60000));
		if (minutes < 60) return `${minutes}m`;
		const hours = Math.round(minutes / 60);
		return hours < 48 ? `${hours}h` : `${Math.round(hours / 24)}d`;
	}

	function bytes(value: string | undefined): string {
		const n = Number(value);
		if (!Number.isFinite(n)) return '';
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		let size = n;
		let unit = 0;
		while (size >= 1024 && unit < units.length - 1) {
			size /= 1024;
			unit++;
		}
		return `${size < 10 ? size.toFixed(1) : Math.round(size)} ${units[unit]}`;
	}

	// The one condition whose numbers are worth spelling out: free space is why a world stops
	// saving with no error anywhere.
	function detail(item: InboxItem): string {
		if (item.kind === 'low_disk') {
			return `${bytes(item.detail?.Free)} free, below the ${bytes(item.detail?.Alarm)} floor`;
		}
		if (item.kind === 'job_failed' || item.kind === 'job_stuck') {
			return [item.detail?.Job, item.detail?.Error, item.detail?.Running]
				.filter(Boolean)
				.join(' · ');
		}
		if (item.kind === 'crash_loop') {
			return `${item.detail?.Stops} stops in ${item.detail?.Window}`;
		}
		if (item.kind === 'stale_backup') {
			return item.detail?.Last ? `last archive ${item.detail.Last}` : 'no archive on record';
		}
		return '';
	}
</script>

{#if items.length > 0}
	<Card.Root>
		<Card.Header>
			<Card.Title class="flex items-center justify-between gap-3">
				<span class="flex items-center gap-2">
					<TriangleAlert aria-hidden="true" />
					Needs attention
				</span>
				<Badge variant={critical > 0 ? 'destructive' : 'outline'}>
					{items.length}
					{items.length === 1 ? 'item' : 'items'}
				</Badge>
			</Card.Title>
		</Card.Header>
		<Card.Content class="grid gap-2">
			{#each items as item (item.kind + (item.instance_id ?? ''))}
				{@const note = detail(item)}
				<Alert.Root variant={item.severity === 'critical' ? 'destructive' : 'default'}>
					<Alert.Title class="flex flex-wrap items-center justify-between gap-2">
						<span>{sentence[item.kind]}</span>
						<span class="text-xs font-normal opacity-70">{since(item)}</span>
					</Alert.Title>
					<Alert.Description>
						<div class="flex flex-wrap items-center justify-between gap-2">
							<span>
								{#if item.instance_name}
									<span class="font-medium">{item.instance_name}</span>
								{:else}
									This host
								{/if}
								{#if note}
									· {note}
								{/if}
							</span>
							{#if item.instance_id}
								<Button
									variant="outline"
									size="sm"
									href={resolve('/instances/[id]', { id: item.instance_id })}
								>
									Open server
								</Button>
							{/if}
						</div>
					</Alert.Description>
				</Alert.Root>
			{/each}
		</Card.Content>
	</Card.Root>
{/if}
