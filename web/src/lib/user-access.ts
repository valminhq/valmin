import type { Grant, GrantRole } from '$lib/api/grants';

/** A server and the grants read from it. */
export interface ServerGrants {
	id: string;
	name: string;
	grants: Grant[];
}

/** A server a member reaches, and the base role their grant gives there. */
export interface ServerReach {
	id: string;
	name: string;
	role: GrantRole;
}

/** The servers a member reaches through a live grant, in the order given. An expired grant
 * reaches nothing. */
export function serversReached(
	userId: string,
	servers: ServerGrants[],
	now = Date.now()
): ServerReach[] {
	return servers.flatMap(({ id, name, grants }) => {
		const grant = grants.find(
			(item) =>
				item.user_id === userId && (item.expires_at === null || Date.parse(item.expires_at) > now)
		);
		return grant ? [{ id, name, role: grant.role }] : [];
	});
}
