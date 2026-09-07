<script lang="ts">
	import { type Instance } from '$lib/api/instances';
	import { schedules, scheduleKinds, type Schedule } from '$lib/api/schedules';
	import { session } from '$lib/state/session.svelte';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import DestructiveConfirm from '$lib/components/destructive-confirm.svelte';
	import Problem from '$lib/components/problem.svelte';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	let { instance }: { instance: Instance } = $props();

	let list = $state<Schedule[]>([]);
	let loading = $state(true);
	let failure = $state<unknown>(null);
	let saving = $state(false);
	let kind = $state('');
	let cron = $state('0 4 * * *');
	let deleting = $state<Schedule | null>(null);
	let deleteOpen = $state(false);

	const allowed = $derived(session.allowed(instance.id));
	/** Each kind is gated on the action its tick would exercise, not on one schedule
	 * capability: a member who may back this server up may schedule a backup of it
	 * (ADR-132). */
	const offered = $derived(scheduleKinds.filter((k) => allowed.includes(k.action)));
	const mine = $derived(list.filter((s) => s.instance_id === instance.id));
	const ready = $derived(kind !== '' && cron.trim() !== '' && !saving);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			list = await schedules.list();
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	async function act(call: () => Promise<unknown>) {
		saving = true;
		failure = null;
		try {
			await call();
			await load();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	function create() {
		void act(async () => {
			await schedules.create({ instance_id: instance.id, kind, cron: cron.trim() });
			kind = '';
		});
	}

	const toggle = (s: Schedule, enabled: boolean) =>
		void act(() => schedules.patch(s.id, { enabled }));

	function remove() {
		const target = deleting;
		if (target) void act(() => schedules.remove(target.id));
	}

	function ask(s: Schedule) {
		deleting = s;
		deleteOpen = true;
	}

	function label(k: string): string {
		return scheduleKinds.find((s) => s.kind === k)?.label ?? k.replaceAll('_', ' ');
	}

	function when(iso: string | null): string {
		return iso ? new Date(iso).toLocaleString() : 'never';
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Schedules</Card.Title>
		<Card.Description>
			Standing instructions for this server. A schedule that comes due while the server is busy
			records a skipped run rather than queuing behind it, so a backlog never builds up.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		{#if offered.length === 0}
			<p class="text-sm text-muted-foreground" data-testid="schedules-blocked">
				Scheduling is not available to you.
			</p>
		{:else}
			<Problem error={failure} />

			{#if loading}
				<p class="text-sm text-muted-foreground">Loading…</p>
			{:else if mine.length === 0}
				<p class="text-sm text-muted-foreground">Nothing is scheduled for this server.</p>
			{:else}
				<div class="grid gap-2">
					{#each mine as s (s.id)}
						<div class="flex flex-wrap items-center gap-3 rounded-lg border p-3">
							<div class="grid flex-1 gap-1">
								<div class="flex flex-wrap items-center gap-2">
									<span class="text-sm font-medium">{label(s.kind)}</span>
									<Badge variant="outline" class="font-mono">{s.cron}</Badge>
									{#if !s.enabled}<Badge variant="secondary">paused</Badge>{/if}
								</div>
								<!--
									The timezone is the daemon's, sent with the row. An operator reading "04:00"
									and assuming their own clock is the misunderstanding this names away.
								-->
								<p class="text-xs text-muted-foreground">
									next {when(s.next_run_at)} · last {when(s.last_run_at)} · times in {s.timezone}
									{#if s.created_by_username}· set up by {s.created_by_username}{/if}
								</p>
							</div>
							<Switch
								checked={s.enabled}
								disabled={saving}
								onCheckedChange={(v) => toggle(s, v)}
								aria-label="Enabled"
							/>
							<Button variant="ghost" size="sm" disabled={saving} onclick={() => ask(s)}>
								<Trash2 />
								<span class="sr-only">Delete schedule</span>
							</Button>
						</div>
					{/each}
				</div>
			{/if}

			<div class="grid gap-3 border-t pt-4 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
				<div class="grid gap-2">
					<Label for="schedule-kind">What to run</Label>
					<Select.Root type="single" bind:value={kind}>
						<Select.Trigger id="schedule-kind">
							{kind ? label(kind) : 'Choose…'}
						</Select.Trigger>
						<Select.Content>
							{#each offered as option (option.kind)}
								<Select.Item value={option.kind}>{option.label}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				</div>
				<div class="grid gap-2">
					<Label for="schedule-cron">When</Label>
					<!--
						The expression is the daemon's to validate: it answers an invalid one with the
						field and its own help text, so there is no second, weaker parser here.
					-->
					<Input id="schedule-cron" bind:value={cron} autocomplete="off" placeholder="0 4 * * *" />
					<p class="text-xs text-muted-foreground">
						Five fields: minute, hour, day of month, month, day of week.
					</p>
				</div>
				<Button size="sm" disabled={!ready} onclick={create}>Add schedule</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>

<!-- F5: deleting a schedule stops something running unattended, so it is named and typed back. -->
<DestructiveConfirm
	bind:open={deleteOpen}
	name={deleting ? deleting.cron : ''}
	title="Delete this schedule?"
	description="{deleting
		? label(deleting.kind)
		: ''} stops running on its own. Runs already in the job history are kept."
	confirmLabel="Delete"
	onconfirm={remove}
/>
