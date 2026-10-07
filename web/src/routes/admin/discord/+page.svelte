<script lang="ts">
	import { discordAdmin, type DiscordLink, type DiscordSettings } from '$lib/api/admin';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { unsaved } from '$lib/state/dirty.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import Plus from '@lucide/svelte/icons/plus';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	/** A link row being edited; key only tells rows apart in the list. */
	type Row = DiscordLink & { key: number };

	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));
	let settings = $state<DiscordSettings | null>(null);
	let servers = $state<Instance[]>([]);
	let enabled = $state(false);
	let token = $state('');
	let rows = $state<Row[]>([]);
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let baseline = $state('');
	let nextKey = 0;

	const draft = $derived(JSON.stringify([enabled, rows.map(({ key: _key, ...link }) => link)]));
	unsaved(() => allowed && !loading && (draft !== baseline || token !== ''));

	const STATE_TEXT: Record<string, string> = {
		disabled: 'Off',
		connecting: 'Connecting…',
		connected: 'Connected',
		failed: 'Stopped'
	};

	function fill(row: DiscordSettings) {
		settings = row;
		enabled = row.enabled;
		token = '';
		rows = row.links.map((link) => ({
			key: nextKey++,
			guild_id: link.guild_id,
			channel_id: link.channel_id,
			allow_start: link.allow_start,
			instance_ids: [...link.instance_ids]
		}));
		baseline = JSON.stringify([enabled, rows.map(({ key: _key, ...link }) => link)]);
	}

	async function load() {
		loading = true;
		try {
			const [row, list] = await Promise.all([discordAdmin.get(), instances.list()]);
			servers = list;
			fill(row);
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}
	$effect(() => {
		if (allowed) void load();
	});

	function addLink() {
		rows.push({
			key: nextKey++,
			guild_id: '',
			channel_id: '',
			allow_start: true,
			instance_ids: []
		});
	}

	function toggleServer(row: Row, id: string, on: boolean) {
		row.instance_ids = on
			? [...row.instance_ids, id]
			: row.instance_ids.filter((other) => other !== id);
	}

	async function save() {
		saving = true;
		failure = null;
		try {
			fill(
				await discordAdmin.save({
					...(token !== '' && { token }),
					enabled,
					links: rows.map(({ key: _key, ...link }) => ({
						...link,
						guild_id: link.guild_id.trim(),
						channel_id: link.channel_id.trim()
					}))
				})
			);
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	async function remove() {
		if (!confirm('Remove the Discord bot? Its token and every link are deleted.')) return;
		failure = null;
		try {
			await discordAdmin.remove();
			await load();
		} catch (err) {
			failure = err;
		}
	}
</script>

<main class="mx-auto grid w-full max-w-3xl gap-6 p-4 sm:p-6">
	<h1 class="text-2xl font-semibold">Discord bot</h1>
	{#if !allowed}
		<p>You do not have permission to manage the Discord bot.</p>
	{:else}
		<p class="text-sm text-muted-foreground">
			Let people in your Discord check and start game servers with <code>/status</code> and
			<code>/start</code>. Each link decides which servers a Discord server or channel can see, and
			whether it may start them. Everyone in a linked channel can use both commands.
		</p>
		<Problem error={failure} />
		{#if loading}<p>Loading Discord settings…</p>
		{:else if settings}
			<Card.Root>
				<Card.Header><Card.Title>Bot</Card.Title></Card.Header>
				<Card.Content class="grid gap-4">
					<p class="text-sm" role="status">
						{STATE_TEXT[settings.status.state] ?? settings.status.state}{settings.status.bot_name
							? ` as ${settings.status.bot_name}`
							: ''}
					</p>
					{#if settings.status.error}
						<p class="text-sm text-destructive">{settings.status.error}</p>
					{/if}
					{#if settings.invite_url}
						<p class="text-sm">
							<!-- An external Discord address, not a panel route. -->
							<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
							<a class="underline" href={settings.invite_url} target="_blank" rel="noreferrer"
								>Invite the bot to a Discord server</a
							>
						</p>
					{/if}
					<div class="grid gap-2">
						<Label for="discord-token">Bot token</Label>
						<Input
							id="discord-token"
							type="password"
							autocomplete="off"
							bind:value={token}
							placeholder={settings.configured ? 'Leave blank to keep the saved token' : ''}
						/>
						<p class="text-xs text-muted-foreground">
							Create an application in the Discord Developer Portal, add a bot, and copy its token.
							Turn off Public Bot there so only you can invite it.
						</p>
					</div>
					<div class="flex items-center gap-3">
						<Switch id="discord-enabled" bind:checked={enabled} />
						<Label for="discord-enabled">Run the bot</Label>
					</div>
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header>
					<Card.Title>Links</Card.Title>
					<Card.Description>
						To copy an ID, turn on Developer Mode in Discord's advanced settings, then right-click
						the Discord server or channel. Leave the channel empty to cover every channel. A
						channel's own link wins over its Discord server's, and a thread follows its channel.
					</Card.Description>
				</Card.Header>
				<Card.Content class="grid gap-6">
					{#each rows as row, i (row.key)}
						<fieldset class="grid gap-3 rounded-md border p-4">
							<legend class="px-1 text-sm font-medium">Link {i + 1}</legend>
							<div class="grid gap-3 sm:grid-cols-2">
								<div class="grid gap-2">
									<Label for="guild-{row.key}">Discord server ID</Label>
									<Input id="guild-{row.key}" inputmode="numeric" bind:value={row.guild_id} />
								</div>
								<div class="grid gap-2">
									<Label for="channel-{row.key}">Channel ID (optional)</Label>
									<Input id="channel-{row.key}" inputmode="numeric" bind:value={row.channel_id} />
								</div>
							</div>
							<div class="grid gap-2">
								<p class="text-sm font-medium">Servers</p>
								{#if servers.length === 0}
									<p class="text-sm text-muted-foreground">There are no servers yet.</p>
								{/if}
								{#each servers as server (server.id)}
									<label class="flex items-center gap-2 text-sm">
										<input
											type="checkbox"
											checked={row.instance_ids.includes(server.id)}
											onchange={(event) =>
												toggleServer(row, server.id, event.currentTarget.checked)}
										/>
										{server.name}
									</label>
								{/each}
							</div>
							<div class="flex items-center gap-3">
								<Switch id="start-{row.key}" bind:checked={row.allow_start} />
								<Label for="start-{row.key}">Allow /start</Label>
							</div>
							<Button
								variant="outline"
								size="sm"
								class="justify-self-start"
								onclick={() => rows.splice(i, 1)}
							>
								<Trash2 class="size-4" /> Remove link
							</Button>
						</fieldset>
					{/each}
					<Button variant="outline" class="justify-self-start" onclick={addLink}>
						<Plus class="size-4" /> Add link
					</Button>
				</Card.Content>
			</Card.Root>

			<div class="flex flex-wrap gap-2">
				<Button disabled={saving} onclick={save}
					>{saving ? 'Saving…' : 'Save Discord settings'}</Button
				>
				{#if settings.configured || settings.links.length > 0}
					<Button variant="outline" disabled={saving} onclick={remove}>Remove bot</Button>
				{/if}
			</div>
		{/if}
	{/if}
</main>
