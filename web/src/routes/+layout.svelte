<script lang="ts">
	import '../app.css';
	import AppHeader from '$lib/components/app-header.svelte';
	import { ModeWatcher } from 'mode-watcher';
	import { beforeNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
	import { base, resolve } from '$app/paths';
	import { session } from '$lib/state/session.svelte';
	import { socket } from '$lib/socket/index.svelte';
	import { discardUnsaved, hasUnsaved } from '$lib/state/dirty.svelte';
	import { loginWithReturn, returnPath } from '$lib/nav';

	let { children } = $props();

	// The published status page belongs to whoever has the link. It is neither signed-in nor
	// sign-in: an operator who is signed in stays on it rather than being sent home.
	const isStatus = $derived(page.url.pathname.startsWith('/status/'));

	const isPublic = $derived(
		page.url.pathname === '/login' ||
			page.url.pathname === '/setup' ||
			page.url.pathname.startsWith('/redeem/')
	);

	$effect(() => {
		void session.load();
	});

	// The socket exists exactly while somebody is signed in. Opening one before that would
	// be rejected at the handshake anyway (`11 §6.3`).
	$effect(() => {
		if (session.user) socket.connect();
		else socket.close();
	});

	$effect(() => {
		if (isStatus || session.loading) return;
		if (session.setupRequired && page.url.pathname !== '/setup') {
			void goto(resolve('/setup'));
			return;
		}
		if (!session.setupRequired && !session.user && !isPublic) {
			// The destination is carried through sign-in, so a saved link to a server section
			// lands there rather than on the list. resolve() takes a known route, not a path
			// built at runtime, so the base prefix is applied here instead.
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			void goto(base + loginWithReturn('/login', page.url));
			return;
		}
		if (session.user && isPublic) {
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			void goto(base + returnPath(page.url), {
				replaceState: page.url.pathname.startsWith('/redeem/')
			});
		}
	});

	// Guards every editor holding unsaved work. The prompt is the browser's own because
	// beforeNavigate is synchronous and the unload path can show nothing else.
	beforeNavigate((nav) => {
		if (!hasUnsaved()) return;
		if (confirm('Leave without saving? The changes on this page will be lost.')) {
			discardUnsaved();
			return;
		}
		nav.cancel();
	});
</script>

<svelte:window
	onbeforeunload={(event) => {
		if (hasUnsaved()) event.preventDefault();
	}}
/>

<svelte:head>
	<title>Valmin</title>
</svelte:head>

<!-- In the root layout, never onMount, or the page paints light before the theme
     applies and every load flashes white (`06 §4`). -->
<ModeWatcher />

{#if session.user && !isPublic && !isStatus}
	<AppHeader />
{/if}
{@render children()}
