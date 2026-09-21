<script lang="ts">
	import { navigationMenu } from '$lib/navigation-menu';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import ChevronDown from '@lucide/svelte/icons/chevron-down';

	let { id }: { id: string } = $props();
	const allowed = $derived(session.allowed(id));
	const overview = $derived(resolve('/instances/[id]', { id }));
	const links = $derived(
		[
			{ href: overview, label: 'Overview', visible: true },
			{
				href: resolve('/instances/[id]/backups', { id }),
				label: 'Backups',
				visible: allowed.includes(actions.backupsList)
			},
			{
				href: resolve('/instances/[id]/mods', { id }),
				label: 'Mods',
				visible: allowed.includes(actions.modsList)
			},
			{
				href: resolve('/instances/[id]/configs', { id }),
				label: 'Settings files',
				visible: allowed.includes(actions.configRead)
			},
			{
				href: resolve('/instances/[id]/players', { id }),
				label: 'Player access',
				visible: allowed.includes(actions.playersManage)
			},
			{
				href: resolve('/instances/[id]/access', { id }),
				label: 'Panel access',
				visible: allowed.includes(actions.grantsManage)
			},
			{ href: resolve('/instances/[id]/settings', { id }), label: 'Server settings', visible: true }
		].filter((link) => link.visible)
	);

	// A section owns its nested pages: the file editor is part of Settings files. Overview is the
	// prefix of every other href, so it matches only itself.
	function isCurrent(href: string): boolean {
		return (
			page.url.pathname === href || (href !== overview && page.url.pathname.startsWith(href + '/'))
		);
	}
	const current = $derived(links.find((link) => isCurrent(link.href)));
</script>

<!-- The surrounding band and its border belong to the server layout, which also carries the
     identity this navigates within. Only one of the two renderings is ever displayed, so the
     section the row marks and the one the menu marks cannot disagree. -->
<nav aria-label="Server sections" class="mx-auto max-w-5xl px-4 sm:px-6">
	<details use:navigationMenu class="relative py-2 sm:hidden">
		<summary
			class="group inline-flex cursor-pointer list-none items-center gap-1.5 rounded-md border px-3 py-2 text-sm font-medium focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring [&::-webkit-details-marker]:hidden"
		>
			<span>{current?.label ?? 'Sections'}</span>
			<ChevronDown class="size-4 transition-transform group-open:rotate-180" />
		</summary>
		<ul
			class="absolute left-4 z-20 mt-1 grid min-w-56 gap-0.5 rounded-md border bg-card p-1 shadow-md"
		>
			{#each links as link (link.href)}
				<li>
					<a
						class="flex rounded-sm px-2 py-2 text-sm hover:bg-muted focus-visible:bg-muted focus-visible:outline-none aria-[current=page]:bg-secondary aria-[current=page]:font-medium"
						href={link.href}
						aria-current={isCurrent(link.href) ? 'page' : undefined}>{link.label}</a
					>
				</li>
			{/each}
		</ul>
	</details>

	<div class="hidden flex-wrap gap-x-5 sm:flex">
		{#each links as link (link.href)}
			{@const active = isCurrent(link.href)}
			<a
				href={link.href}
				aria-current={active ? 'page' : undefined}
				class="border-b-2 px-1 py-3 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring {active
					? 'border-primary text-foreground'
					: 'border-transparent text-muted-foreground hover:border-border hover:text-foreground'}"
				>{link.label}</a
			>
		{/each}
	</div>
</nav>
