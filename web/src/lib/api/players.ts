import { api, type JSONResource } from './client';

export type PlayerListKind = 'admins' | 'bans' | 'permitted';

export interface PlayerList {
	ids: string[];
}

function path(instanceId: string, kind: PlayerListKind): string {
	return `/instances/${instanceId}/${kind}`;
}

/** One point in the observed history. `players` null is an observation gap — the panel could
 * not tell, which is not the same fact as nobody being connected — and must render as a gap,
 * never as zero. */
export interface PlayerObservation {
	id: string;
	observed_at: string;
	players: number | null;
}

interface Page<T> {
	items: T[];
	next_cursor: string | null;
}

export const playerHistory = {
	/** Newest first, keyset-paginated. Authorized on `stats.read`, like the live count. */
	list: (instanceId: string, limit = 200) =>
		api.get<Page<PlayerObservation>>(`/instances/${instanceId}/players/history?limit=${limit}`)
};

/** One account the server named in its log. `name` is empty for the lines that carry no
 * name, and the id is still the useful half. Seen, not played: the line most sightings come
 * from is a connection attempt, and one measured instance of it was rejected for a wrong
 * password. */
export interface SeenPlayer {
	platform_id: string;
	name: string;
	first_seen_at: string;
	last_seen_at: string;
}

export const seenPlayers = {
	/** One row per account, most recent first. Authorized on `players.manage`, like the
	 * three lists it is the raw material for. */
	list: (instanceId: string) => api.get<Page<SeenPlayer>>(`/instances/${instanceId}/players/seen`)
};

export const playerLists = {
	get: (instanceId: string, kind: PlayerListKind): Promise<JSONResource<PlayerList>> =>
		api.getJSON<PlayerList>(path(instanceId, kind)),
	put: (
		instanceId: string,
		kind: PlayerListKind,
		ids: string[],
		etag: string
	): Promise<JSONResource<PlayerList>> =>
		api.putJSON<PlayerList>(path(instanceId, kind), { ids }, etag)
};
