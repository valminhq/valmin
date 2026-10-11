<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api/errors';
	import type { Job } from '$lib/api/types';
	import {
		rcloneSetups,
		type RcloneProvider,
		type RcloneSetup,
		type RcloneSetupInput
	} from '$lib/api/remote-backups';
	import { leave } from '$lib/nav';
	import { discardUnsaved, hasUnsaved } from '$lib/state/dirty.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import CopyButton from '$lib/components/copy-button.svelte';
	import Field from '$lib/components/field.svelte';
	import JobProgress from '$lib/components/job-progress.svelte';
	import Problem from '$lib/components/problem.svelte';

	/**
	 * Creates a saved rclone connection: provider fields, browser authorization for OAuth
	 * providers, OneDrive's drive choice, then the save job. Authorization leaves the page;
	 * the callback page brings the administrator back with `resumeId`.
	 */
	let {
		open = $bindable(false),
		resumeId = null,
		oncreated
	}: {
		open?: boolean;
		resumeId?: string | null;
		oncreated: (name: string) => void;
	} = $props();

	const PROVIDERS: { value: RcloneProvider; label: string }[] = [
		{ value: 'drive', label: 'Google Drive' },
		{ value: 'dropbox', label: 'Dropbox' },
		{ value: 'onedrive', label: 'OneDrive' },
		{ value: 'mega', label: 'MEGA' },
		{ value: 's3', label: 'S3-compatible storage' }
	];
	const OAUTH_HINTS: Partial<Record<RcloneProvider, string>> = {
		drive:
			'Create a Web application OAuth client in Google Cloud and add this callback URL as an authorized redirect URI. Publish the app: while it is in testing, Google expires its access after seven days.',
		dropbox:
			'Create a Dropbox app, add this callback URL as a redirect URI, and enable the files.metadata.write, files.content.write, files.content.read, sharing.write and account_info.read permissions.',
		onedrive:
			'Register an app in Microsoft Entra that allows personal accounts, add this callback URL as a Web redirect URI, and create a client secret.'
	};
	const S3_PROVIDERS = ['AWS', 'Cloudflare', 'Wasabi', 'Minio', 'Other'];
	const providerLabel = (value: RcloneProvider) =>
		PROVIDERS.find((p) => p.value === value)?.label ?? value;

	let provider = $state<RcloneProvider>('drive');
	let name = $state('drive-backups');
	let clientId = $state('');
	let clientSecret = $state('');
	let user = $state('');
	let password = $state('');
	let s3Provider = $state('Other');
	let endpoint = $state('');
	let region = $state('');
	let accessKeyId = $state('');
	let secretAccessKey = $state('');
	let sessionToken = $state('');

	let setup = $state<RcloneSetup | null>(null);
	let driveId = $state('');
	let jobId = $state<string | null>(null);
	let jobError = $state('');
	let busy = $state(false);
	let failure = $state<unknown>(null);

	const oauth = $derived(provider in OAUTH_HINTS);
	const callbackURL = $derived(
		setup?.callback_url ?? page.url.origin + resolve('/admin/rclone-callback')
	);
	const fieldError = (field: string) =>
		failure instanceof ApiError ? failure.field(field) : undefined;

	$effect(() => {
		const id = resumeId;
		if (open && id) untrack(() => void resume(id));
	});

	function choose(next: RcloneProvider) {
		if (name === `${provider}-backups`) name = `${next}-backups`;
		provider = next;
	}

	function input(): RcloneSetupInput {
		switch (provider) {
			case 'mega':
				return { provider, name, user, password };
			case 's3':
				return {
					provider,
					name,
					s3_provider: s3Provider,
					endpoint,
					region,
					access_key_id: accessKeyId,
					secret_access_key: secretAccessKey,
					session_token: sessionToken
				};
			default:
				return { provider, name, client_id: clientId, client_secret: clientSecret };
		}
	}

	async function run(action: () => Promise<void>) {
		busy = true;
		failure = null;
		jobError = '';
		try {
			await action();
		} catch (err) {
			failure = err;
			if (err instanceof ApiError && err.code === 'not_found') setup = null;
		} finally {
			busy = false;
		}
	}

	const resume = (id: string) =>
		run(async () => {
			setup = await rcloneSetups.get(id);
			provider = setup.provider;
			name = setup.name;
		});

	const start = () =>
		run(async () => {
			setup = await rcloneSetups.start(input());
			if (setup.step === 'authorize') connect();
			else await save();
		});

	/** Leaves for the provider's consent page; the callback page returns here. */
	function connect() {
		if (!setup?.authorization_url) return;
		discardUnsaved();
		leave(setup.authorization_url);
	}

	const chooseDrive = () =>
		run(async () => {
			if (setup) setup = await rcloneSetups.advance(setup.id, { drive_id: driveId });
		});

	async function save() {
		if (setup) jobId = (await rcloneSetups.finish(setup.id)).job_id;
	}

	function saved(job: Job) {
		jobId = null;
		if (job.status !== 'succeeded' || !setup) {
			jobError = job.error || 'The connection could not be saved.';
			return;
		}
		const created = setup.name;
		reset();
		open = false;
		oncreated(created);
	}

	function reset() {
		provider = 'drive';
		name = 'drive-backups';
		[clientId, clientSecret, user, password] = ['', '', '', ''];
		[s3Provider, endpoint, region] = ['Other', '', ''];
		[accessKeyId, secretAccessKey, sessionToken] = ['', '', ''];
		setup = null;
		driveId = '';
		jobId = null;
		jobError = '';
		failure = null;
	}

	function close() {
		// Cancelling is a courtesy: an abandoned setup expires on its own.
		if (setup && !jobId) rcloneSetups.cancel(setup.id).catch(() => undefined);
		reset();
		open = false;
	}
</script>

<Dialog.Root bind:open onOpenChange={(next) => !next && close()}>
	<Dialog.Content class="sm:max-w-lg">
		<Dialog.Header>
			<Dialog.Title>Create connection</Dialog.Title>
			<Dialog.Description>
				Save a new rclone connection. Choose it as the destination afterwards, then test it.
			</Dialog.Description>
		</Dialog.Header>
		<Problem error={failure} />
		{#if jobError}<p class="text-sm text-destructive" role="alert">{jobError}</p>{/if}

		{#if jobId}
			<JobProgress {jobId} onfinish={saved} />
		{:else if setup?.step === 'choose_drive'}
			<fieldset class="grid gap-2">
				<legend class="mb-2 text-sm font-medium">Drive for {setup.name}</legend>
				{#each setup.drives ?? [] as drive (drive.id)}
					<label class="flex items-center gap-2 text-sm">
						<input type="radio" name="rclone-drive" value={drive.id} bind:group={driveId} />
						{drive.name} <span class="text-muted-foreground">({drive.type})</span>
					</label>
				{/each}
			</fieldset>
		{:else if setup?.step === 'ready'}
			<p class="text-sm">
				{providerLabel(setup.provider)} is connected. Save
				<span class="font-mono">{setup.name}</span> to add it to the rclone configuration.
			</p>
		{:else if setup?.step === 'authorize'}
			<p class="text-sm">
				Connect your {providerLabel(setup.provider)} account to continue setting up
				<span class="font-mono">{setup.name}</span>.
			</p>
		{:else}
			<form
				id="rclone-connection"
				class="grid gap-4"
				onsubmit={(event) => {
					event.preventDefault();
					void start();
				}}
			>
				<div class="grid gap-2">
					<Label for="rclone-provider">Provider</Label>
					<select
						id="rclone-provider"
						value={provider}
						onchange={(event) => choose(event.currentTarget.value as RcloneProvider)}
						class="h-10 rounded-md border bg-background px-3"
					>
						{#each PROVIDERS as option (option.value)}
							<option value={option.value}>{option.label}</option>
						{/each}
					</select>
				</div>
				<Field
					id="rclone-name"
					label="Connection name"
					required
					hint="Letters, digits, hyphens and underscores."
					error={fieldError('name')}
				>
					{#snippet children(field)}
						<Input id="rclone-name" required bind:value={name} {...field} />
					{/snippet}
				</Field>

				{#if oauth}
					<Field id="rclone-client-id" label="Client ID" required error={fieldError('client_id')}>
						{#snippet children(field)}
							<Input
								id="rclone-client-id"
								required
								autocomplete="off"
								bind:value={clientId}
								{...field}
							/>
						{/snippet}
					</Field>
					<Field
						id="rclone-client-secret"
						label="Client secret"
						required
						error={fieldError('client_secret')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-client-secret"
								type="password"
								required
								autocomplete="off"
								bind:value={clientSecret}
								{...field}
							/>
						{/snippet}
					</Field>
					<section class="grid gap-2 rounded-md border p-3" aria-labelledby="rclone-callback">
						<h3 id="rclone-callback" class="text-sm font-medium">Callback URL</h3>
						<code class="rounded bg-muted px-3 py-2 text-sm break-all">{callbackURL}</code>
						<CopyButton value={callbackURL} label="Copy callback URL" size="sm" />
						<p class="text-sm text-muted-foreground">{OAUTH_HINTS[provider]}</p>
					</section>
					{#if hasUnsaved()}
						<p class="text-sm text-muted-foreground">
							Connecting opens the provider's sign-in page. Unsaved destination changes on this page
							are discarded.
						</p>
					{/if}
				{:else if provider === 'mega'}
					<Field id="rclone-user" label="MEGA email" required error={fieldError('user')}>
						{#snippet children(field)}
							<Input
								id="rclone-user"
								type="email"
								required
								autocomplete="off"
								bind:value={user}
								{...field}
							/>
						{/snippet}
					</Field>
					<Field
						id="rclone-password"
						label="MEGA password"
						required
						hint="Accounts with two-factor authentication cannot sign in unattended."
						error={fieldError('password')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-password"
								type="password"
								required
								autocomplete="off"
								bind:value={password}
								{...field}
							/>
						{/snippet}
					</Field>
				{:else}
					<Field
						id="rclone-s3-provider"
						label="S3 provider"
						required
						hint="The provider name rclone uses, or Other."
						error={fieldError('s3_provider')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-s3-provider"
								list="rclone-s3-providers"
								required
								bind:value={s3Provider}
								{...field}
							/>
							<datalist id="rclone-s3-providers">
								{#each S3_PROVIDERS as option (option)}<option value={option}></option>{/each}
							</datalist>
						{/snippet}
					</Field>
					<Field
						id="rclone-endpoint"
						label="Endpoint"
						hint="HTTPS address of the storage service. Optional for AWS."
						error={fieldError('endpoint')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-endpoint"
								type="url"
								placeholder="https://s3.example.com"
								bind:value={endpoint}
								{...field}
							/>
						{/snippet}
					</Field>
					<Field id="rclone-region" label="Region" error={fieldError('region')}>
						{#snippet children(field)}
							<Input id="rclone-region" bind:value={region} {...field} />
						{/snippet}
					</Field>
					<Field
						id="rclone-access-key"
						label="Access key ID"
						required
						error={fieldError('access_key_id')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-access-key"
								required
								autocomplete="off"
								bind:value={accessKeyId}
								{...field}
							/>
						{/snippet}
					</Field>
					<Field
						id="rclone-secret-key"
						label="Secret access key"
						required
						error={fieldError('secret_access_key')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-secret-key"
								type="password"
								required
								autocomplete="off"
								bind:value={secretAccessKey}
								{...field}
							/>
						{/snippet}
					</Field>
					<Field
						id="rclone-session-token"
						label="Session token"
						error={fieldError('session_token')}
					>
						{#snippet children(field)}
							<Input
								id="rclone-session-token"
								type="password"
								autocomplete="off"
								bind:value={sessionToken}
								{...field}
							/>
						{/snippet}
					</Field>
				{/if}
			</form>
		{/if}

		<Dialog.Footer>
			<Button type="button" variant="outline" onclick={close}>Cancel</Button>
			{#if !jobId}
				{#if setup?.step === 'choose_drive'}
					<Button type="button" disabled={!driveId || busy} onclick={chooseDrive}
						>Use this drive</Button
					>
				{:else if setup?.step === 'ready'}
					<Button type="button" disabled={busy} onclick={() => run(save)}>Save connection</Button>
				{:else if setup?.step === 'authorize'}
					<Button type="button" disabled={busy} onclick={connect}>Connect account</Button>
				{:else}
					<Button type="submit" form="rclone-connection" disabled={busy}>
						{oauth ? 'Connect account' : 'Save connection'}
					</Button>
				{/if}
			{/if}
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
