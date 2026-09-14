<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';

	let { id }: { id: string } = $props();
	const allowed = $derived(session.allowed(id));
	const overview = $derived(resolve('/instances/[id]', { id }));
	const links = $derived([
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
	]);
</script>

<div class="border-b bg-card">
	<nav aria-label="Server sections" class="mx-auto flex max-w-5xl flex-wrap gap-x-5 px-6">
		{#each links.filter((link) => link.visible) as link (link.href)}
			{@const active =
				page.url.pathname === link.href ||
				(link.href !== overview && page.url.pathname.startsWith(link.href + '/'))}
			<a
				href={link.href}
				aria-current={active ? 'page' : undefined}
				class="border-b-2 px-1 py-3 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring {active
					? 'border-primary text-foreground'
					: 'border-transparent text-muted-foreground hover:border-border hover:text-foreground'}"
				>{link.label}</a
			>
		{/each}
	</nav>
</div>
