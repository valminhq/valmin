<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import {
		alertRuleAdmin,
		webhookAdmin,
		type AlertRule,
		type GameKind,
		type Webhook
	} from '$lib/api/admin';
	import { actions, instances, type Instance } from '$lib/api/instances';
	import { GAME_EVENTS, MAX_TEMPLATE, expand, isGameKind, type GameEvent } from '$lib/game-events';
	import { session } from '$lib/state/session.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Switch } from '$lib/components/ui/switch';
	import Problem from '$lib/components/problem.svelte';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';

	interface Draft {
		enabled: boolean;
		template: string;
	}
	type Drafts = Record<GameKind, Draft>;

	const GROUPS = ['Players', 'Bosses', 'Raids'] as const;
	const GROUP_NOTE: Record<(typeof GROUPS)[number], string> = {
		Players: 'A player’s joins and leaves are posted at most once a minute.',
		Bosses: 'Bosses are found from the game itself, so modded bosses are reported too.',
		Raids: 'A raid already running when the server restarts is not posted again.'
	};
	const PLUGIN_CONFIG = 'io.valmin.eye.cfg';
	const textareaClass =
		'w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 aria-invalid:border-destructive aria-invalid:ring-destructive/20';

	const id = $derived(page.params.id ?? '');
	// Rendered from allowed_actions, never from a role name (F3).
	const allowed = $derived(session.allowedGlobally().includes(actions.panelSettings));

	let instance = $state<Instance | null>(null);
	// The installed ValminEye version: null without it, undefined while unknown.
	let plugin = $state<string | null | undefined>(undefined);
	let destinations = $state<Webhook[]>([]);
	let rules = $state<AlertRule[]>([]);
	let loading = $state(true);
	let saving = $state(false);
	let saved = $state(false);
	let failure = $state<unknown>(null);

	let drafts = $state<Drafts>(fromRules([]));
	let postTo = $state<string[]>([]);
	// What was last loaded, to tell whether anything is unsaved.
	let baseline = $state('');

	const serverRules = $derived(
		rules.filter((r) => r.instance_id === id && isGameKind(r.condition_kind))
	);
	const snapshot = $derived(JSON.stringify({ drafts, postTo: [...postTo].sort() }));
	const dirty = $derived(!loading && snapshot !== baseline);
	const anyOn = $derived(GAME_EVENTS.some((e) => drafts[e.kind].enabled));
	const tooLong = $derived(GAME_EVENTS.some((e) => drafts[e.kind].template.length > MAX_TEMPLATE));
	const blocked = $derived(
		anyOn && postTo.length === 0
			? 'Choose at least one channel to post to.'
			: tooLong
				? `A message can be at most ${MAX_TEMPLATE} characters.`
				: ''
	);

	$effect(() => {
		void load(id);
	});

	async function load(instanceId: string) {
		try {
			const [inst, hooks, all] = await Promise.all([
				instances.get(instanceId),
				webhookAdmin.list(),
				alertRuleAdmin.list()
			]);
			instance = inst;
			destinations = hooks;
			rules = all;
			reset();
			failure = null;
			void instances.capabilities(instanceId).then(
				(c) => (plugin = c.valmin_eye),
				() => (plugin = undefined)
			);
		} catch (err) {
			failure = err;
		} finally {
			loading = false;
		}
	}

	function ruleFor(kind: GameKind, from: AlertRule[] = serverRules) {
		return from.find((r) => r.condition_kind === kind);
	}

	/** The form as the stored rules describe it. A rule without a template shows the default. */
	function fromRules(from: AlertRule[]): Drafts {
		return Object.fromEntries(
			GAME_EVENTS.map((e) => {
				const rule = ruleFor(e.kind, from);
				return [
					e.kind,
					{ enabled: rule?.enabled ?? false, template: rule?.params.template || e.template }
				];
			})
		) as Drafts;
	}

	/** Puts the form back to what is stored. With one channel and nothing set up yet, it is preselected. */
	function reset() {
		drafts = fromRules(serverRules);
		postTo = serverRules[0]
			? [...serverRules[0].webhook_ids]
			: destinations.length === 1
				? [destinations[0].id]
				: [];
		baseline = snapshot;
	}

	async function save() {
		saving = true;
		saved = false;
		failure = null;
		try {
			for (const e of GAME_EVENTS) {
				const draft = drafts[e.kind];
				const template = draft.template.trim();
				// The default is stored as no template, so a better default reaches this server too.
				const params = template && template !== e.template ? { template } : {};
				const rule = ruleFor(e.kind);
				if (rule) {
					await alertRuleAdmin.update(rule.id, {
						enabled: draft.enabled,
						params,
						webhook_ids: postTo
					});
				} else if (draft.enabled) {
					await alertRuleAdmin.create({
						condition_kind: e.kind,
						instance_id: id,
						webhook_ids: postTo,
						params
					});
				}
			}
			rules = await alertRuleAdmin.list();
			reset();
			saved = true;
		} catch (err) {
			failure = err;
		} finally {
			saving = false;
		}
	}

	/** Inserts {name} where the cursor is in an event's message. */
	function insert(kind: GameKind, name: string) {
		const box = document.getElementById(`message-${kind}`) as HTMLTextAreaElement | null;
		const text = drafts[kind].template;
		const at = box?.selectionStart ?? text.length;
		const end = box?.selectionEnd ?? text.length;
		const token = `{${name}}`;
		drafts[kind].template = text.slice(0, at) + token + text.slice(end);
		requestAnimationFrame(() => {
			box?.focus();
			box?.setSelectionRange(at + token.length, at + token.length);
		});
	}

	const preview = (e: GameEvent) =>
		expand(drafts[e.kind].template.trim() || e.template, {
			...e.sample,
			server: instance?.name ?? 'This server'
		});
	const toggled = (list: string[], item: string, on: boolean) =>
		on ? [...list, item] : list.filter((other) => other !== item);

	$effect(() => {
		if (dirty) saved = false;
	});
</script>

<div class="grid gap-6 pb-24">
	<header class="grid gap-1">
		<h2 class="text-2xl font-semibold tracking-tight">Game events</h2>
		<p class="max-w-prose text-sm text-muted-foreground">
			Post what happens in the game to your Discord channels: who joins and leaves, boss fights, and
			raids. Players don’t need to install anything.
		</p>
	</header>

	<Problem error={failure} />

	{#if !allowed}
		<p class="text-sm text-muted-foreground">
			Only panel administrators can set up game events, because they choose where the panel posts.
		</p>
	{:else if loading}
		<p class="text-sm text-muted-foreground">Loading…</p>
	{:else}
		<section
			class="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-lg border p-4"
			data-testid="plugin-status"
		>
			{#if plugin === undefined}
				<p class="text-sm text-muted-foreground">Checking for the ValminEye plugin…</p>
			{:else if plugin === null}
				<div class="grid flex-1 gap-1">
					<p class="text-sm font-medium">This server needs the ValminEye plugin</p>
					<p class="text-sm text-muted-foreground">
						The plugin notices these events inside the game. Install it, then restart the server.
						You can set up messages now.
					</p>
				</div>
				<Button href={resolve(`/instances/[id]/mods?q=ValminEye`, { id })}>Install ValminEye</Button
				>
			{:else}
				<p class="flex-1 text-sm">ValminEye {plugin} is installed.</p>
				<Button
					variant="outline"
					size="sm"
					href={resolve('/instances/[id]/configs/[file]', { id, file: PLUGIN_CONFIG })}
				>
					Plugin settings
				</Button>
			{/if}
		</section>

		<section class="grid gap-2">
			<h3 class="text-sm font-medium">Post to</h3>
			{#if destinations.length === 0}
				<p class="text-sm text-muted-foreground">
					No channels yet. Add a Discord webhook on the
					<a class="underline" href={resolve('/admin/webhooks')}>Notifications</a> page, then come back.
				</p>
			{:else}
				<div class="flex flex-wrap gap-2">
					{#each destinations as d (d.id)}
						<label
							class="flex cursor-pointer items-center gap-2 rounded-md border px-3 py-1.5 text-sm has-[:checked]:border-primary has-[:checked]:bg-primary/5 has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring"
						>
							<input
								type="checkbox"
								class="accent-primary"
								checked={postTo.includes(d.id)}
								onchange={(ev) => (postTo = toggled(postTo, d.id, ev.currentTarget.checked))}
							/>
							{d.name}
						</label>
					{/each}
				</div>
			{/if}
		</section>

		{#each GROUPS as group (group)}
			<section class="grid gap-3" aria-labelledby="group-{group}">
				<div class="grid gap-0.5">
					<h3 id="group-{group}" class="text-lg font-semibold tracking-tight">{group}</h3>
					<p class="text-sm text-muted-foreground">{GROUP_NOTE[group]}</p>
				</div>
				<ul class="divide-y rounded-lg border">
					{#each GAME_EVENTS.filter((e) => e.group === group) as e (e.kind)}
						{@const draft = drafts[e.kind]}
						<li class="grid gap-3 p-4" data-testid="event-{e.kind}">
							<div class="flex items-start gap-3">
								<Switch
									id="toggle-{e.kind}"
									class="mt-0.5"
									bind:checked={drafts[e.kind].enabled}
									aria-describedby="when-{e.kind}"
								/>
								<div class="grid gap-0.5">
									<label for="toggle-{e.kind}" class="text-sm font-medium">{e.label}</label>
									<p id="when-{e.kind}" class="text-sm text-muted-foreground">{e.when}</p>
								</div>
							</div>

							{#if draft.enabled}
								<div class="grid gap-3 sm:pl-12">
									<div class="grid gap-2">
										<div class="flex items-center justify-between gap-2">
											<label for="message-{e.kind}" class="text-sm font-medium">Message</label>
											{#if draft.template.trim() !== e.template && draft.template.trim() !== ''}
												<Button
													variant="ghost"
													size="sm"
													class="h-7"
													onclick={() => (drafts[e.kind].template = e.template)}
												>
													<RotateCcw /> Use the default
												</Button>
											{/if}
										</div>
										<textarea
											id="message-{e.kind}"
											rows="2"
											class={textareaClass}
											aria-invalid={draft.template.length > MAX_TEMPLATE}
											bind:value={drafts[e.kind].template}></textarea>
										<div class="flex flex-wrap items-center gap-1.5">
											<span class="text-xs text-muted-foreground">Insert:</span>
											{#each e.placeholders as name (name)}
												<button
													type="button"
													class="rounded border bg-muted/50 px-1.5 py-0.5 font-mono text-xs hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
													onclick={() => insert(e.kind, name)}
												>
													{`{${name}}`}
												</button>
											{/each}
										</div>
									</div>

									<figure class="grid gap-1.5">
										<figcaption class="text-xs text-muted-foreground">
											In Discord it looks like this
										</figcaption>
										<div
											class="grid gap-0.5 rounded-md border-l-4 bg-muted/60 px-3 py-2"
											style:border-left-color={e.accent}
											data-testid="preview-{e.kind}"
										>
											<span class="text-xs font-medium text-muted-foreground">
												{instance?.name ?? 'This server'}
											</span>
											<span class="text-sm font-semibold">{e.headline}</span>
											<span class="text-sm break-words whitespace-pre-wrap">{preview(e)}</span>
										</div>
									</figure>
								</div>
							{/if}
						</li>
					{/each}
				</ul>
			</section>
		{/each}

		{#if saved && !dirty}
			<p class="text-sm text-muted-foreground" role="status">Changes saved.</p>
		{/if}
	{/if}
</div>

{#if allowed && dirty}
	<div
		class="fixed inset-x-0 bottom-0 z-20 border-t bg-background/95 px-4 py-3 backdrop-blur"
		role="region"
		aria-label="Unsaved changes"
	>
		<div class="mx-auto flex max-w-5xl flex-wrap items-center justify-end gap-3">
			<p class="mr-auto text-sm {blocked ? 'text-destructive' : 'text-muted-foreground'}">
				{blocked || 'You have unsaved changes.'}
			</p>
			<Button variant="outline" disabled={saving} onclick={reset}>Discard</Button>
			<Button disabled={saving || blocked !== ''} onclick={save}>
				{saving ? 'Saving…' : 'Save changes'}
			</Button>
		</div>
	</div>
{/if}
