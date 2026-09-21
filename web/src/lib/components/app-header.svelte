<script lang="ts">
	import { navigationMenu } from '$lib/navigation-menu';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import { socketStatus } from '$lib/socket/index.svelte';
	import { Button } from '$lib/components/ui/button';
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import LogOut from '@lucide/svelte/icons/log-out';
	import UserRoundCog from '@lucide/svelte/icons/user-round-cog';
	import Link from '@lucide/svelte/icons/link';
	import ScrollText from '@lucide/svelte/icons/scroll-text';
	import BellRing from '@lucide/svelte/icons/bell-ring';
	import KeyRound from '@lucide/svelte/icons/key-round';
	import Stethoscope from '@lucide/svelte/icons/stethoscope';

	// Visibility comes from the granted actions, never from a role name (`09 §4`).
	const granted = $derived(session.allowedGlobally());
	const canAdminPanel = $derived(granted.includes(actions.panelSettings));
	const adminLinks = $derived(
		[
			{
				href: resolve('/admin/users'),
				label: 'Users',
				icon: UserRoundCog,
				visible: granted.includes(actions.usersManage)
			},
			{
				href: resolve('/admin/invites'),
				label: 'Invites',
				icon: Link,
				visible: granted.includes(actions.invitesManage)
			},
			{
				href: resolve('/admin/audit'),
				label: 'Audit log',
				icon: ScrollText,
				visible: granted.includes(actions.auditRead)
			},
			{
				href: resolve('/admin/webhooks'),
				label: 'Notifications',
				icon: BellRing,
				visible: canAdminPanel
			},
			{
				href: resolve('/admin/keys'),
				label: 'Encryption keys',
				icon: KeyRound,
				visible: canAdminPanel
			},
			{
				href: resolve('/admin/diagnostics'),
				label: 'Diagnostics',
				icon: Stethoscope,
				visible: canAdminPanel
			}
		].filter((link) => link.visible)
	);
	const openAdmin = $derived(adminLinks.find((link) => link.href === page.url.pathname));
	const home = $derived(page.url.pathname === resolve('/'));

	// A menu left open across the navigation it started covers the page it arrived at, so each
	// closes on the click that leaves it. Every control inside one navigates.
	let admin = $state(false);
	let account = $state(false);

	async function signOut() {
		try {
			await api.post('/auth/logout');
		} finally {
			instanceList.release();
			session.signedOut();
			await goto(resolve('/login'));
		}
	}

	const summary =
		'group inline-flex cursor-pointer list-none items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium hover:bg-muted focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring [&::-webkit-details-marker]:hidden';
	const panel =
		'absolute right-0 z-20 mt-1 grid min-w-52 gap-0.5 rounded-md border bg-card p-1 shadow-md';
	const item =
		'flex items-center gap-2 rounded-sm px-2 py-2 text-sm hover:bg-muted focus-visible:bg-muted focus-visible:outline-none aria-[current=page]:bg-secondary aria-[current=page]:font-medium';
</script>

<header class="flex flex-wrap items-center gap-2 border-b bg-card px-4 py-3 sm:px-6">
	<a class="text-lg font-semibold tracking-tight" href={resolve('/')}>Valmin</a>
	<Button
		variant={home ? 'secondary' : 'ghost'}
		size="sm"
		aria-current={home ? 'page' : undefined}
		href={resolve('/')}>Servers</Button
	>
	<nav
		aria-label="Administration and account"
		class="flex w-full min-w-0 flex-wrap items-center justify-end gap-1 sm:ml-auto sm:w-auto"
	>
		{#if socketStatus.value !== 'open'}
			<span role="status" class="rounded-md bg-muted px-2 py-1 text-xs text-muted-foreground">
				{socketStatus.value === 'connecting' ? 'reconnecting…' : 'offline'}
			</span>
		{/if}
		{#if adminLinks.length > 0}
			<details use:navigationMenu class="relative" bind:open={admin}>
				<summary class={summary}>
					<span class="sm:hidden">Admin</span><span class="hidden sm:inline"
						>{openAdmin ? openAdmin.label : 'Administration'}</span
					>
					<ChevronDown class="size-4 transition-transform group-open:rotate-180" />
				</summary>
				<ul class={panel}>
					{#each adminLinks as link (link.href)}
						<li>
							<a
								class={item}
								href={link.href}
								aria-current={link.href === page.url.pathname ? 'page' : undefined}
								onclick={() => (admin = false)}
							>
								<link.icon class="size-4" aria-hidden="true" />
								{link.label}
							</a>
						</li>
					{/each}
				</ul>
			</details>
		{/if}
		<details use:navigationMenu class="relative" bind:open={account}>
			<summary class={summary}>
				<span class="max-w-20 truncate sm:max-w-32">{session.user?.username ?? 'Account'}</span>
				<ChevronDown class="size-4 transition-transform group-open:rotate-180" />
			</summary>
			<div class={panel}>
				<p class="max-w-60 px-2 py-1.5 text-xs break-words text-muted-foreground">
					Signed in as {session.user?.username ?? ''}
				</p>
				<button class={item} type="button" onclick={signOut}>
					<LogOut class="size-4" aria-hidden="true" /> Sign out
				</button>
			</div>
		</details>
	</nav>
</header>
