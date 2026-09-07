import { api, type JSONResource } from './client';

export type PlayerListKind = 'admins' | 'bans' | 'permitted';

export interface PlayerList {
	ids: string[];
}

function path(instanceId: string, kind: PlayerListKind): string {
	return `/instances/${instanceId}/${kind}`;
}

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
