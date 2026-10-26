<script lang="ts">
	import type { CreateWebhook, NotificationsApi, Webhook } from '#lib/api/notifications.js';
	import { formatInstant } from '#lib/api/schedules.js';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import * as Select from '#lib/components/ui/select/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import Problem from '#lib/components/problem.svelte';
	import Send from '@lucide/svelte/icons/send';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	/** The webhooks of one scope with their switches, and the form that adds one. `server` is the
	 * server whose own webhooks these are, absent for the shared ones; on a server, shared rows
	 * are listed read-only. `onchange` runs after every change. */
	let {
		api,
		channels,
		server,
		onchange
	}: {
		api: NotificationsApi;
		channels: Webhook[];
		server?: string;
		onchange: () => void;
	} = $props();

	let name = $state('');
	let kind = $state<CreateWebhook['kind']>('discord');
	let url = $state('');
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let testJob = $state<string | null>(null);
	let deleting = $state<Webhook | null>(null);
	let deleteOpen = $state(false);

	const ready = $derived(name.trim() !== '' && url.trim() !== '' && !saving);
	const editable = (c: Webhook) => (c.instance_id ?? undefined) === server;

	async function act(call: () => Promise<unknown>) {
		saving = true;
		failure = null;
		try {
			await call();
			onchange();
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	const add = () =>
		act(async () => {
			await api.webhooks.create({ name: name.trim(), kind, url: url.trim() });
			name = '';
			url = '';
		});
	const test = (c: Webhook) =>
		act(async () => {
			testJob = (await api.webhooks.test(c.id)).job_id;
		});
	const remove = () => {
		const target = deleting;
		if (target) void act(() => api.webhooks.remove(target.id));
	};
</script>

<div class="grid gap-4">
	<Problem error={failure} />

	{#if channels.length === 0}
		<p class="text-sm text-muted-foreground">
			No channels yet. Add one below, then send a test notification.
		</p>
	{:else}
		<div class="grid gap-2">
			{#each channels as c (c.id)}
				<div class="grid gap-3 rounded-lg border p-3" data-testid="channel-{c.id}">
					<div class="flex flex-wrap items-center gap-3">
						<div class="grid flex-1 gap-1">
							<div class="flex flex-wrap items-center gap-2">
								<span class="text-sm font-medium">{c.name}</span>
								<Badge variant="outline">{c.kind}</Badge>
								{#if server && c.instance_id === null}<Badge variant="secondary">shared</Badge>{/if}
								{#if !c.enabled}<Badge variant="secondary">paused</Badge>{/if}
							</div>
							<p class="text-sm text-muted-foreground">
								{editable(c)
									? `added ${formatInstant(c.created_at)}`
									: 'Every server can post here. Managed in Administration › Notifications.'}
							</p>
						</div>
						{#if editable(c)}
							<Button variant="outline" size="sm" disabled={saving} onclick={() => test(c)}>
								<Send /> Send test notification
							</Button>
							<Switch
								checked={c.enabled}
								disabled={saving}
								onCheckedChange={(enabled) => act(() => api.webhooks.update(c.id, { enabled }))}
								aria-label="Enable notifications for {c.name}"
							/>
							<Button
								variant="ghost"
								size="sm"
								disabled={saving}
								onclick={() => {
									deleting = c;
									deleteOpen = true;
								}}
							>
								<Trash2 />
								<span class="sr-only">Delete {c.name}</span>
							</Button>
						{/if}
					</div>
					{#if editable(c)}
						<div class="flex items-start gap-2">
							<Switch
								id="catch-all-{c.id}"
								checked={c.catch_all}
								disabled={saving}
								onCheckedChange={(catch_all) => act(() => api.webhooks.update(c.id, { catch_all }))}
							/>
							<div class="grid gap-0.5">
								<Label for="catch-all-{c.id}">Also post built-in alerts</Label>
								<p class="text-xs text-muted-foreground">
									{server
										? 'This server stopping unexpectedly or failing a backup'
										: 'Unexpected stops, failed backups and new game builds'}, unless a rule already
									posts them here.
								</p>
							</div>
						</div>
					{/if}
				</div>
			{/each}
		</div>
	{/if}

	{#if testJob}
		<div class="rounded-lg border p-4">
			<JobProgress jobId={testJob} onfinish={onchange} />
		</div>
	{/if}

	<div class="grid gap-3 border-t pt-4">
		<div class="grid gap-3 sm:grid-cols-[1fr_10rem]">
			<div class="grid gap-2">
				<Label for="webhook-name">Name</Label>
				<Input id="webhook-name" bind:value={name} placeholder="Friends’ Discord" />
			</div>
			<div class="grid gap-2">
				<Label for="webhook-kind">Receiver</Label>
				<Select.Root type="single" bind:value={kind}>
					<Select.Trigger id="webhook-kind">
						{kind === 'discord' ? 'Discord' : 'Generic JSON'}
					</Select.Trigger>
					<Select.Content>
						<Select.Item value="discord">Discord</Select.Item>
						<Select.Item value="generic">Generic JSON</Select.Item>
					</Select.Content>
				</Select.Root>
			</div>
		</div>
		<div class="grid gap-2">
			<Label for="webhook-url">URL</Label>
			<Input id="webhook-url" bind:value={url} placeholder="https://discord.com/api/webhooks/…" />
			<p class="text-sm text-muted-foreground">
				Must be an <code>https://</code> address on the public internet. Addresses on this machine or
				its network are refused, and the panel does not follow redirects.
			</p>
		</div>
		<Button class="justify-self-start" disabled={!ready} onclick={add}>Add channel</Button>
	</div>
</div>

<DestructiveConfirm
	bind:open={deleteOpen}
	name={deleting?.name ?? ''}
	title="Delete this channel?"
	description={`${deleting?.name ?? 'It'} stops receiving notifications, and the rules posting to it stop posting there. Its delivery history goes with it, and the URL cannot be recovered.`}
	confirmLabel="Delete channel"
	onconfirm={remove}
/>
