<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { api } from '$lib/api/client';
	import { session } from '$lib/state/session.svelte';
	import { socketStatus } from '$lib/socket/index.svelte';
	import { Button } from '$lib/components/ui/button';
	import LogOut from '@lucide/svelte/icons/log-out';

	async function signOut() {
		try {
			await api.post('/auth/logout');
		} finally {
			session.signedOut();
			await goto(resolve('/login'));
		}
	}
</script>

<div class="min-h-screen">
	<header class="flex items-center justify-between border-b px-6 py-3">
		<span class="font-semibold">Valmin</span>
		<div class="flex items-center gap-3">
			{#if socketStatus.value !== 'open'}
				<span class="text-xs text-muted-foreground">
					{socketStatus.value === 'connecting' ? 'reconnecting…' : 'offline'}
				</span>
			{/if}
			<span class="text-sm text-muted-foreground">{session.user?.username ?? ''}</span>
			<Button variant="ghost" size="icon-sm" onclick={signOut} aria-label="Sign out">
				<LogOut />
			</Button>
		</div>
	</header>

	<main class="p-6">
		<p class="text-sm text-muted-foreground">
			<!-- The instance list and the create wizard are WP-23; this is the shell they hang
			     off. -->
			No servers yet.
		</p>
	</main>
</div>
