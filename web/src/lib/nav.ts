import { actions } from '#lib/api/instances.js';
import { scheduleKinds } from '#lib/api/schedules.js';

/** The parts of a location the helpers read, which both URL and SvelteKit's readonly page.url satisfy. */
type Location = Pick<URL, 'origin' | 'pathname' | 'search'> & {
	searchParams: Pick<URLSearchParams, 'get'>;
};

/**
 * The path a visitor was trying to reach before being sent to sign in, carried through as
 * `?next=`. Only a path on this origin is honoured: anything that resolves elsewhere, however
 * it is spelled, falls back to the server list.
 */
export function returnPath(url: Location): `/${string}` {
	const next = url.searchParams.get('next');
	if (!next || !next.startsWith('/')) return '/';
	const target = new URL(next, url.origin);
	if (target.origin !== url.origin) return '/';
	return `${target.pathname}${target.search}${target.hash}` as `/${string}`;
}

/** The sign-in URL, carrying the path being left so it can be returned to. */
export function loginWithReturn(login: `/${string}`, from: Location): `/${string}` {
	const next = from.pathname + from.search;
	if (next === '/') return login;
	return `${login}?next=${encodeURIComponent(next)}`;
}

/** Leaves the panel for an external page, such as a storage provider's sign-in. */
export function leave(url: string): void {
	window.location.assign(url);
}

/** A page of one server: its path segment under `/instances/{id}` (empty for the overview) and its tab label. */
export interface ServerSection {
	segment: string;
	label: string;
}

/** The server tabs, in order, that the actions held on the server reach. Compare is not a tab;
 * see `canCompare`. */
export function serverSections(allowed: readonly string[]): ServerSection[] {
	const holds = (action: string) => allowed.includes(action);
	return [
		{ segment: '', label: 'Overview', visible: true },
		{ segment: 'mods', label: 'Mods', visible: holds(actions.modsList) },
		{ segment: 'configs', label: 'Mod config', visible: holds(actions.configRead) },
		{
			segment: 'players',
			label: 'Players',
			visible: holds(actions.playersManage) || holds(actions.statsRead)
		},
		{
			segment: 'notifications',
			label: 'Notifications',
			visible: holds(actions.notificationsManage)
		},
		{ segment: 'backups', label: 'Backups', visible: holds(actions.backupsList) },
		{ segment: 'setups', label: 'Saved setups', visible: holds(actions.setupsManage) },
		{
			segment: 'maintenance',
			label: 'Maintenance',
			visible: scheduleKinds.some((kind) => holds(kind.action)) || holds(actions.worldTools)
		},
		{ segment: 'access', label: 'Panel access', visible: holds(actions.grantsManage) },
		{ segment: 'settings', label: 'Settings', visible: true }
	]
		.filter((section) => section.visible)
		.map(({ segment, label }) => ({ segment, label }));
}

/** Whether the held actions reach the comparison, which reads a server's settings, mods and configuration. */
export function canCompare(allowed: readonly string[]): boolean {
	return [actions.settings, actions.modsList, actions.configRead].every((action) =>
		allowed.includes(action)
	);
}

/**
 * Where choosing server `to` from a page of server `from` lands: the same section when the
 * actions held on `to` reach it, otherwise the overview of `to`. Anything after the section
 * (a file name, a query) belongs to the old server and is dropped.
 */
export function switchServerPath(
	pathname: string,
	from: string,
	to: string,
	allowedOnTarget: readonly string[]
): `/${string}` {
	const overview = `/instances/${to}` as const;
	const prefix = `/instances/${from}/`;
	if (!pathname.startsWith(prefix)) return overview;
	const segment = pathname.slice(prefix.length).split('/')[0];
	const reachable =
		segment === 'compare'
			? canCompare(allowedOnTarget)
			: serverSections(allowedOnTarget).some((section) => section.segment === segment);
	return segment !== '' && reachable ? `${overview}/${segment}` : overview;
}
