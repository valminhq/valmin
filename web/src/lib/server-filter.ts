import type { InboxItem } from '$lib/api/inbox';
import type { Instance } from '$lib/api/instances';
import { chipsFor } from '$lib/conditions';

/** The state filter on the server list. */
export type ServerStateFilter = 'all' | 'running' | 'stopped' | 'attention';

/**
 * The servers whose name contains the query, ignoring case and surrounding whitespace, and
 * whose state passes the filter. A server needs attention when it is in the error state or
 * carries a condition chip. The order given is the order returned.
 */
export function filterServers(
	servers: Instance[],
	conditions: InboxItem[],
	query: string,
	state: ServerStateFilter
): Instance[] {
	const needle = query.trim().toLowerCase();
	return servers.filter((server) => {
		if (!server.name.toLowerCase().includes(needle)) return false;
		if (state === 'all') return true;
		if (state === 'attention') {
			return server.state === 'error' || chipsFor(conditions, server.id).length > 0;
		}
		return server.state === state;
	});
}
