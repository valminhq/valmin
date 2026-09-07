import { api } from './client';

export type GrantRole = 'viewer' | 'operator';

export interface Grant {
	user_id: string;
	instance_id: string;
	role: GrantRole;
	perms: string[];
	granted_by: string | null;
	granted_at: string;
	expires_at: string | null;
	etag: string;
}

export interface GrantRoleOption {
	role: GrantRole;
	allowed_actions: string[];
}

export interface GrantExtraOption {
	action: string;
	risk: string;
}

export interface GrantPage {
	items: Grant[];
	next_cursor: string | null;
	roles: GrantRoleOption[];
	extra_capabilities: GrantExtraOption[];
}

export interface ReplaceGrant {
	role: GrantRole;
	perms: string[];
}

function path(instanceId: string, userId?: string): string {
	const base = `/instances/${encodeURIComponent(instanceId)}/grants`;
	return userId ? `${base}/${encodeURIComponent(userId)}` : base;
}

export const grants = {
	list: (instanceId: string) => api.get<GrantPage>(path(instanceId)),
	get: (instanceId: string, userId: string) => api.getJSON<Grant>(path(instanceId, userId)),
	create: (instanceId: string, userId: string, body: ReplaceGrant) =>
		api.createJSON<Grant>(path(instanceId, userId), body),
	replace: (instanceId: string, userId: string, body: ReplaceGrant, etag: string) =>
		api.putJSON<Grant>(path(instanceId, userId), body, etag),
	remove: (instanceId: string, userId: string, etag: string) =>
		api.deleteJSON(path(instanceId, userId), etag)
};
