<script lang="ts">
	import '../app.css';
	import AppHeader from '#lib/components/app-header.svelte';
	import { ModeWatcher } from 'mode-watcher';
	import { beforeNavigate, goto } from '$app/navigation';
	import { navigating, page, updated } from '$app/state';
	import { resolve } from '$app/paths';
	import ReloadNotice from '#lib/components/reload-notice.svelte';
	import DestructiveConfirm from '#lib/components/destructive-confirm.svelte';
	import { loading, navigationStarted } from '#lib/state/busy.svelte.js';
	import { session } from '#lib/state/session.svelte.js';
	import { instanceList } from '#lib/state/instances.svelte.js';
	import { socket } from '#lib/socket/index.svelte.js';
	import { discardUnsaved, hasUnsaved } from '#lib/state/dirty.svelte.js';
	import { leave, loginWithReturn, returnPath } from '#lib/nav.js';

	let { children } = $props();

	// The published status page belongs to whoever has the link. It is neither signed-in nor
	// sign-in: an operator who is signed in stays on it rather than being sent home.
	const isStatus = $derived(page.url.pathname.startsWith('/status/'));

	/**
	 * What each route calls itself in the document title. One table rather than a head block
	 * per route drifts, and a nested `<title>` would render twice.
	 * A route left out keeps whatever title it sets for itself.
	 */
	const SECTION: Record<string, string> = {
		'/': 'Servers',
		'/instances/new': 'New server',
		'/instances/import': 'New from template',
		'/instances/[id]': 'Overview',
		'/instances/[id]/backups': 'Backups',
		'/instances/[id]/setups': 'Saved setups',
		'/instances/[id]/maintenance': 'Maintenance',
		'/instances/[id]/mods': 'Mods',
		'/instances/[id]/configs': 'Mod configuration',
		'/instances/[id]/configs/[file]': 'Mod configuration',
		'/instances/[id]/players': 'Players',
		'/instances/[id]/access': 'Panel access',
		'/instances/[id]/compare': 'Compare servers',
		'/instances/[id]/settings': 'Server settings',
		'/instances/[id]/clone': 'Clone server',
		'/instances/[id]/notifications': 'Notifications',
		'/instances/adopt/[container_id]': 'Recover container',
		'/login': 'Sign in',
		'/setup': 'Set up Valmin',
		'/admin/users': 'Users',
		'/admin/invites': 'Invites',
		'/admin/players': 'Players',
		'/admin/audit': 'Audit log',
		'/admin/webhooks': 'Notifications',
		'/admin/shutdowns': 'Power cuts',
		'/admin/discord': 'Discord bot',
		'/admin/keys': 'Encryption keys',
		'/admin/export': 'Export installation',
		'/admin/cache': 'Cache',
		'/admin/remote-backups': 'Remote backups',
		'/admin/rclone-callback': 'Remote backups',
		'/admin/diagnostics': 'Diagnostics',
		'/account/password': 'Change password',
		'/account/sessions': 'Signed-in devices',
		'/account/date-time': 'Date and time'
	};

	// Two servers open on the same section are told apart by their names, which is the whole
	// point of putting one in the title.
	const serverName = $derived(
		instanceList.items.find((instance) => instance.id === page.params.id)?.name ?? ''
	);
	const section = $derived(SECTION[page.route.id ?? '']);
	const title = $derived([serverName, section, 'Valmin'].filter(Boolean).join(' · '));

	const isPublic = $derived(
		page.url.pathname === '/login' ||
			page.url.pathname === '/setup' ||
			page.url.pathname.startsWith('/redeem/')
	);

	// The public status page is for anonymous visitors and never asks who is signed in.
	$effect(() => {
		if (!isStatus) void session.load();
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
			void goto(resolve('setup'));
			return;
		}
		if (!session.setupRequired && !session.user && !isPublic) {
			// The destination is carried through sign-in, so a saved link to a server section
			// lands there rather than on the list. resolve() takes a known route, not a path
			// built at runtime.
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			void goto(loginWithReturn('/login', page.url));
			return;
		}
		if (
			session.user?.password_change_required &&
			!isPublic &&
			page.url.pathname !== '/account/password'
		) {
			void goto(resolve('account/password'), { replaceState: true });
			return;
		}
		if (session.user && isPublic) {
			// A path that matches no route is refused by goto and opened with a full page load.
			const path = returnPath(page.url);
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			void goto(path, { replace: page.url.pathname.startsWith('/redeem/') }).catch(() =>
				leave(path)
			);
		}
	});

	// A panel upgrade restarts the daemon, so a reconnect is the moment to ask whether this tab
	// is still running the current build.
	$effect(() => socket.onConnected(() => void updated.check()));

	// Where a link or goto was headed when unsaved work stopped it, while the panel asks.
	let leaving = $state<URL | null>(null);
	// A link to the page already open replaces it, as Kit itself would.
	let leavingSamePage = false;
	let confirmLeave = $state(false);

	// Guards every editor holding unsaved work. A link or goto inside the panel is held and the
	// panel's own dialog asks; leaving the page, and Back or Forward, use the browser's prompt,
	// since beforeNavigate is synchronous there and cancelling Back makes Kit step history again.
	// Once a newer build exists, a link or goto to another page becomes a full page load so it
	// lands on that build. Back and Forward stay client-side; so do query-only changes, which
	// callers make with replace and keep focus for.
	beforeNavigate((nav) => {
		if (!nav.shallow) navigationStarted();
		if (nav.shallow && nav.type === 'goto') return;
		if (hasUnsaved()) {
			if (!nav.willUnload && nav.type !== 'popstate' && nav.to) {
				nav.cancel();
				leaving = nav.to.url;
				leavingSamePage = nav.to.url.href === nav.from?.url.href;
				confirmLeave = true;
				return;
			}
			if (!confirm('Leave without saving? The changes on this page will be lost.')) {
				nav.cancel();
				return;
			}
			discardUnsaved();
		}
		if (!updated.current || nav.willUnload || nav.type === 'popstate' || !nav.to) return;
		if (nav.to.url.pathname === nav.from?.url.pathname) return;
		nav.cancel();
		leave(nav.to.url.href);
	});
</script>

<svelte:window
	onbeforeunload={(event) => {
		if (hasUnsaved()) event.preventDefault();
	}}
/>

<svelte:head>
	{#if section}
		<title>{title}</title>
	{/if}
</svelte:head>

<!-- In the root layout, never onMount, or the page paints light before the theme
     applies and every load flashes white (`06 §4`). -->
<ModeWatcher disableHeadScriptInjection />

<!-- The first stop for a keyboard, so the global and server navigation can be stepped over
     rather than traversed on every page. -->
<a
	href="#main"
	class="sr-only rounded-md bg-background p-3 text-sm font-medium underline focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:ring-2 focus:ring-ring"
>
	Skip to content
</a>

{#if loading.count > 0 || navigating.to}
	<div class="loading-bar" role="progressbar" aria-label="Loading the page"></div>
{/if}
{#if session.user && !isPublic && !isStatus}
	<AppHeader />
{/if}
<ReloadNotice />
<DestructiveConfirm
	bind:open={confirmLeave}
	title="Leave without saving?"
	description="The changes on this page will be lost."
	confirmLabel="Leave"
	cancelLabel="Stay"
	onconfirm={() => {
		const to = leaving;
		leaving = null;
		discardUnsaved();
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		if (to) void goto(to.pathname + to.search + to.hash, { replaceState: leavingSamePage });
	}}
/>
<div id="main" tabindex="-1">
	{@render children()}
</div>

<style>
	/* Shows only for a load that outlasts the delay, so a fast page never flashes it. */
	.loading-bar {
		position: fixed;
		top: env(safe-area-inset-top, 0px);
		left: 0;
		z-index: 60;
		height: 2px;
		width: 100%;
		background: var(--primary);
		opacity: 0;
		transform-origin: left;
		animation:
			loading-show 0s linear 300ms forwards,
			loading-slide 1.4s ease-in-out 300ms infinite;
	}

	@keyframes loading-show {
		to {
			opacity: 1;
		}
	}

	@keyframes loading-slide {
		0% {
			transform: translateX(-100%) scaleX(0.3);
		}
		100% {
			transform: translateX(100%) scaleX(0.3);
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.loading-bar {
			animation: loading-show 0s linear 300ms forwards;
		}
	}
</style>
