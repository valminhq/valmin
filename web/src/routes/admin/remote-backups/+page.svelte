<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { formatInstant } from '#lib/api/schedules.js';
	import {
		remoteBackups,
		type RemoteDestination,
		type DestinationInput
	} from '#lib/api/remote-backups.js';
	import { actions } from '#lib/api/instances.js';
	import { session } from '#lib/state/session.svelte.js';
	import { unsaved } from '#lib/state/dirty.svelte.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import * as Card from '#lib/components/ui/card/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import { Label } from '#lib/components/ui/label/index.js';
	import { Switch } from '#lib/components/ui/switch/index.js';
	import Problem from '#lib/components/problem.svelte';
	import JobProgress from '#lib/components/job-progress.svelte';
	import RcloneConnectionDialog from '#lib/components/rclone-connection-dialog.svelte';

	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));
	let destination = $state<RemoteDestination | null>(null);
	let kind = $state<DestinationInput['kind']>('webdav');
	let enabled = $state(false);
	let endpoint = $state('');
	let username = $state('');
	let password = $state('');
	let remoteName = $state('');
	let folder = $state('');
	let names = $state<string[]>([]);
	let loading = $state(true);
	let saving = $state(false);
	let failure = $state<unknown>(null);
	let jobId = $state<string | null>(null);
	let testing = $state(false);
	let baseline = $state('');
	let connecting = $state(false);
	let resumeId = $state<string | null>(null);
	const fields = $derived(JSON.stringify([kind, enabled, endpoint, username, remoteName, folder]));
	unsaved(() => allowed && !loading && (fields !== baseline || password !== ''));

	function fill(row: RemoteDestination | null) {
		destination = row;
		kind = row?.kind ?? 'webdav';
		enabled = row?.enabled ?? false;
		endpoint = row?.endpoint ?? '';
		username = row?.username ?? '';
		remoteName = row?.remote_name ?? '';
		folder = row?.folder ?? '';
		password = '';
		baseline = JSON.stringify([kind, enabled, endpoint, username, remoteName, folder]);
	}
	async function load() {
		loading = true;
		try {
			fill(await remoteBackups.destination());
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
	// The OAuth callback page returns here with ?setup= to finish a connection.
	$effect(() => {
		const id = page.url.searchParams.get('setup');
		if (!allowed || !id) return;
		untrack(() => {
			resumeId = id;
			connecting = true;
			const url = new URL(page.url.href);
			url.searchParams.delete('setup');
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			void goto(url, { replace: true, reset: false });
		});
	});
	function createConnection() {
		resumeId = null;
		connecting = true;
	}
	async function created(name: string) {
		resumeId = null;
		kind = 'rclone';
		remoteName = name;
		await listRemotes();
	}
	async function listRemotes() {
		try {
			names = (await remoteBackups.remotes()).items;
			failure = null;
		} catch (err) {
			failure = err;
		}
	}
	async function save() {
		saving = true;
		failure = null;
		try {
			const body: DestinationInput = {
				kind,
				enabled,
				folder,
				endpoint: kind === 'webdav' ? endpoint : '',
				username: kind === 'webdav' ? username : '',
				remote_name: kind === 'rclone' ? remoteName : ''
			};
			if (kind === 'webdav' && (password !== '' || !destination?.has_credentials))
				body.password = password;
			fill(await remoteBackups.save(body));
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}
	async function test() {
		testing = true;
		try {
			jobId = (await remoteBackups.test()).job_id;
			failure = null;
		} catch (err) {
			failure = err;
			testing = false;
		}
	}
	function tested() {
		testing = false;
		void load();
	}
	const when = (value: string | null | undefined) => (value ? formatInstant(value) : 'Never');
</script>

<main class="mx-auto grid w-full max-w-3xl gap-6 p-4 sm:p-6">
	<h1 class="text-2xl font-semibold">Remote backups</h1>
	{#if !allowed}
		<p>You do not have permission to manage remote storage.</p>
	{:else}
		<p class="text-sm text-muted-foreground">
			Keep world archives on another host. Configure one destination, then enable automatic uploads
			on each server’s Backups page.
		</p>
		<Problem error={failure} />
		{#if loading}<p>Loading remote storage…</p>
		{:else}
			<Card.Root>
				<Card.Header><Card.Title>Destination</Card.Title></Card.Header>
				<Card.Content>
					<form
						class="grid gap-4"
						onsubmit={(event) => {
							event.preventDefault();
							void save();
						}}
					>
						<div class="grid gap-2">
							<Label for="remote-kind">Storage connection</Label>
							<select
								id="remote-kind"
								bind:value={kind}
								class="h-10 rounded-md border bg-background px-3"
							>
								<option value="webdav">WebDAV</option><option value="rclone">rclone</option>
							</select>
						</div>
						{#if kind === 'webdav'}
							<div class="grid gap-2">
								<Label for="remote-endpoint">WebDAV URL</Label><Input
									id="remote-endpoint"
									type="url"
									required
									placeholder="https://cloud.example.com/remote.php/dav/files/user"
									bind:value={endpoint}
								/>
							</div>
							<div class="grid gap-2">
								<Label for="remote-user">Username</Label><Input
									id="remote-user"
									autocomplete="username"
									required
									bind:value={username}
								/>
							</div>
							<div class="grid gap-2">
								<Label for="remote-password">App password</Label><Input
									id="remote-password"
									type="password"
									autocomplete="new-password"
									bind:value={password}
									placeholder={destination?.has_credentials
										? 'Leave blank to keep saved password'
										: ''}
								/>
							</div>
						{:else}
							<p class="text-sm text-muted-foreground">
								Create a connection for Google Drive, Dropbox, OneDrive, MEGA or S3-compatible
								storage, or use a remote already configured with rclone.
							</p>
							<div class="grid gap-2">
								<Label for="remote-name">Configured remote</Label>
								<Input
									id="remote-name"
									list="remote-names"
									required
									bind:value={remoteName}
									placeholder="drive-backups"
								/>
								<datalist id="remote-names"
									>{#each names as name (name)}<option value={name}></option>{/each}</datalist
								>
								<div class="flex flex-wrap gap-2">
									<Button type="button" variant="outline" onclick={listRemotes}
										>Load configured remotes</Button
									>
									<Button type="button" variant="outline" onclick={createConnection}
										>Create connection</Button
									>
								</div>
							</div>
						{/if}
						<div class="grid gap-2">
							<Label for="remote-folder">Destination folder</Label><Input
								id="remote-folder"
								bind:value={folder}
								placeholder={kind === 'rclone' ? 'bucket-or-folder/backups' : 'Backups'}
							/>
							<p class="text-xs text-muted-foreground">
								Relative to the connection root. For S3, include the bucket name.
							</p>
						</div>
						<div class="flex items-center gap-3">
							<Switch id="remote-enabled" bind:checked={enabled} /><Label for="remote-enabled"
								>Enable uploads and remote retention</Label
							>
						</div>
						<p class="text-sm text-muted-foreground">
							Changing connection or folder starts a new backup history. For WebDAV, enter the app
							password again when changing the URL, username, or folder. Pending copies are
							cancelled; previous remote files stay untouched. Active transfers must finish first.
						</p>
						<div class="flex flex-wrap gap-2">
							<Button type="submit" disabled={saving || testing}
								>{saving ? 'Saving…' : 'Save destination'}</Button
							>
							<Button
								type="button"
								variant="outline"
								disabled={!destination ||
									saving ||
									testing ||
									fields !== baseline ||
									password !== ''}
								onclick={test}>Test connection</Button
							>
						</div>
					</form>
				</Card.Content>
			</Card.Root>
			{#if jobId}<JobProgress {jobId} onfinish={tested} />{/if}
			{#if destination}
				<Card.Root
					><Card.Header><Card.Title>Remote storage health</Card.Title></Card.Header>
					<Card.Content class="grid gap-2 text-sm">
						<p>Last connection test: {when(destination.last_test_at)}</p>
						{#if destination.last_test_error}<p role="status" class="text-destructive">
								{destination.last_test_error}
							</p>{/if}
						<p>Last successful copy: {when(destination.summary.last_success_at)}</p>
						<p>Newest copied archive: {when(destination.summary.last_archive_at)}</p>
						<p>
							{destination.summary.pending} pending · {destination.summary.failed} failed · {destination
								.summary.cleanup_pending} awaiting remote cleanup
						</p>
					</Card.Content></Card.Root
				>
			{/if}
		{/if}
		<RcloneConnectionDialog bind:open={connecting} {resumeId} oncreated={created} />
	{/if}
</main>
