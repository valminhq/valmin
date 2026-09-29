import { actions } from '$lib/api/instances';
import { scheduleKinds } from '$lib/api/schedules';

/**
 * The path a visitor was trying to reach before being sent to sign in, carried through as
 * `?next=`. Only a same-origin absolute path is honoured: anything else — an absolute URL, a
 * protocol-relative `//host`, a backslash the browser normalises to one — falls back to the
 * server list, so the parameter cannot be used to bounce someone off this origin.
 */
export function returnPath(url: URL): `/${string}` {
	const next = url.searchParams.get('next');
	if (!next || !next.startsWith('/')) return '/';
	if (next.startsWith('//') || next.startsWith('/\\')) return '/';
	return next as `/${string}`;
}

/** The sign-in URL, carrying the path being left so it can be returned to. */
export function loginWithReturn(login: `/${string}`, from: URL): `/${string}` {
	const next = from.pathname + from.search;
	if (next === '/') return login;
	return `${login}?next=${encodeURIComponent(next)}`;
}

/** A page of one server: its path segment under `/instances/{id}` (empty for the overview) and its tab label. */
export interface ServerSection {
	segment: string;
	label: string;
}

/** The server tabs, in order, that the held actions reach. Compare is not a tab; see `canCompare`. */
export function serverSections(allowed: readonly string[]): ServerSection[] {
	const holds = (action: string) => allowed.includes(action);
	return [
		{ segment: '', label: 'Overview', visible: true },
		{ segment: 'backups', label: 'Backups', visible: holds(actions.backupsList) },
		{
			segment: 'maintenance',
			label: 'Maintenance',
			visible: scheduleKinds.some((kind) => holds(kind.action))
		},
		{ segment: 'mods', label: 'Mods', visible: holds(actions.modsList) },
		{ segment: 'configs', label: 'Mod configuration', visible: holds(actions.configRead) },
		{ segment: 'players', label: 'Player access', visible: holds(actions.playersManage) },
		{ segment: 'access', label: 'Panel access', visible: holds(actions.grantsManage) },
		{ segment: 'settings', label: 'Server settings', visible: true }
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
