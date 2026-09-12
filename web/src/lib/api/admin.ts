import { api } from './client';
import type { GrantRole } from './grants';
import type { Job, Role, User } from './types';

interface Page<T> {
	items: T[];
}

export interface CreateUser {
	username: string;
	password: string;
	role: Role;
}

export interface UpdateUser {
	role?: Role;
	disabled?: boolean;
}

export interface Invite {
	id: string;
	created_by: string;
	instance_id: string | null;
	grant_role: GrantRole | null;
	grant_perms: string[];
	expires_at: string;
	created_at: string;
	redeemed_at: string | null;
	redeemed_by: string | null;
	revoked_at: string | null;
}

export interface IssueInvite {
	instance_id: string | null;
	grant_role: GrantRole | null;
	grant_perms: string[];
}

export interface IssuedInvite {
	token: string;
	url: string;
	expires_at: string;
}

export const userAdmin = {
	list: () => api.get<Page<User>>('/users').then((page) => page.items),
	create: (body: CreateUser) => api.post<User>('/users', body),
	update: (id: string, body: UpdateUser) =>
		api.patch<User>(`/users/${encodeURIComponent(id)}`, body),
	remove: (id: string) => api.del<void>(`/users/${encodeURIComponent(id)}`),
	resetPassword: (id: string) =>
		api.post<{ password: string }>(`/users/${encodeURIComponent(id)}/password/reset`)
};

export const inviteAdmin = {
	list: () => api.get<Page<Invite>>('/invites').then((page) => page.items),
	issue: (body: IssueInvite) => api.post<IssuedInvite>('/invites', body),
	revoke: (id: string) => api.del<void>(`/invites/${encodeURIComponent(id)}`),
	redeem: (token: string, username: string, password: string) =>
		api.post<User>(`/invites/${encodeURIComponent(token)}/redeem`, { username, password })
};

/** One row of the permanent trail (`04 §3`). `actor` and `instance` are null once the user
 * or instance the row describes is gone; the ids stay. */
export interface AuditEntry {
	id: string;
	user_id: string | null;
	actor: string | null;
	instance_id: string | null;
	instance: string | null;
	action: string;
	detail: string | null;
	ip: string | null;
	created_at: string;
}

/** The filter allowlist the daemon accepts. Anything else is a 400. */
export interface AuditFilter {
	instance_id?: string;
	user_id?: string;
	action?: string;
}

export interface AuditPage {
	items: AuditEntry[];
	next_cursor: string | null;
}

export const auditLog = {
	list: (filter: AuditFilter, cursor?: string) => {
		const query = new URLSearchParams();
		for (const [key, value] of Object.entries(filter)) if (value) query.set(key, value);
		if (cursor) query.set('cursor', cursor);
		return api.get<AuditPage>(`/audit?${query}`);
	}
};

export const keyAdmin = {
	/** Publishes a new derived-key generation and re-encrypts every stored secret under it
	 * (`10 §3.3`). Retrying after an interrupted run continues the same generation. */
	rotate: () => api.post<Job>('/admin/keys/rotate')
};
