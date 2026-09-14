<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { actions } from '$lib/api/instances';
	import { session } from '$lib/state/session.svelte';
	import { instanceList } from '$lib/state/instances.svelte';
	import { socketStatus } from '$lib/socket/index.svelte';
	import { Button } from '$lib/components/ui/button';
	import LogOut from '@lucide/svelte/icons/log-out';
	import UserRoundCog from '@lucide/svelte/icons/user-round-cog';
	import Link from '@lucide/svelte/icons/link';
	import ScrollText from '@lucide/svelte/icons/scroll-text';
	import BellRing from '@lucide/svelte/icons/bell-ring';
	import KeyRound from '@lucide/svelte/icons/key-round';
	const canManageUsers = $derived(session.allowedGlobally().includes(actions.usersManage));
	const canManageInvites = $derived(session.allowedGlobally().includes(actions.invitesManage));
	const canReadAudit = $derived(session.allowedGlobally().includes(actions.auditRead));
	const canAdminPanel = $derived(session.allowedGlobally().includes(actions.panelSettings));
	async function signOut() {
		try {
			await api.post('/auth/logout');
		} finally {
			instanceList.release();
			session.signedOut();
			await goto(resolve('/login'));
		}
	}
</script>

<header class="flex flex-wrap items-center gap-4 border-b bg-card px-6 py-3">
	<a class="text-lg font-semibold tracking-tight" href={resolve('/')}>Valmin</a>
	<Button
		variant={page.url.pathname === resolve('/') ? 'secondary' : 'ghost'}
		aria-current={page.url.pathname === resolve('/') ? 'page' : undefined}
		href={resolve('/')}>Servers</Button
	>
	<nav aria-label="Administration and account" class="ml-auto flex flex-wrap items-center gap-1">
		{#if canManageUsers}
			<Button
				variant={page.url.pathname === resolve('/admin/users') ? 'secondary' : 'ghost'}
				size="sm"
				aria-current={page.url.pathname === resolve('/admin/users') ? 'page' : undefined}
				href={resolve('/admin/users')}
			>
				<UserRoundCog /> Users
			</Button>
		{/if}
		{#if canManageInvites}
			<Button
				variant={page.url.pathname === resolve('/admin/invites') ? 'secondary' : 'ghost'}
				size="sm"
				aria-current={page.url.pathname === resolve('/admin/invites') ? 'page' : undefined}
				href={resolve('/admin/invites')}
			>
				<Link /> Invites
			</Button>
		{/if}
		{#if canReadAudit}
			<Button
				variant={page.url.pathname === resolve('/admin/audit') ? 'secondary' : 'ghost'}
				size="sm"
				aria-current={page.url.pathname === resolve('/admin/audit') ? 'page' : undefined}
				href={resolve('/admin/audit')}
			>
				<ScrollText /> Audit log
			</Button>
		{/if}
		{#if canAdminPanel}
			<Button
				variant={page.url.pathname === resolve('/admin/webhooks') ? 'secondary' : 'ghost'}
				size="sm"
				aria-current={page.url.pathname === resolve('/admin/webhooks') ? 'page' : undefined}
				href={resolve('/admin/webhooks')}
			>
				<BellRing /> Notifications
			</Button>
			<Button
				variant={page.url.pathname === resolve('/admin/keys') ? 'secondary' : 'ghost'}
				size="sm"
				aria-current={page.url.pathname === resolve('/admin/keys') ? 'page' : undefined}
				href={resolve('/admin/keys')}
			>
				<KeyRound /> Encryption keys
			</Button>
		{/if}
		{#if socketStatus.value !== 'open'}
			<span role="status" class="rounded-md bg-muted px-2 py-1 text-xs text-muted-foreground">
				{socketStatus.value === 'connecting' ? 'reconnecting…' : 'offline'}
			</span>
		{/if}
		<span class="text-sm text-muted-foreground">{session.user?.username ?? ''}</span>
		<Button variant="ghost" size="icon-sm" onclick={signOut} aria-label="Sign out">
			<LogOut />
		</Button>
	</nav>
</header>
