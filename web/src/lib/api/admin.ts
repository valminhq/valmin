import { api } from './client';
import type { GrantRole } from './grants';
import type { Role, User } from './types';

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
