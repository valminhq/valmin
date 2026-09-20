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

/** A notification destination (`04 §3`). The URL is never returned: it is a bearer
 * credential, so the panel takes one and reports only what it is called. */
export interface Webhook {
	id: string;
	name: string;
	kind: 'discord' | 'generic';
	enabled: boolean;
	created_at: string;
	updated_at: string;
}

export interface CreateWebhook {
	name: string;
	kind: 'discord' | 'generic';
	url: string;
}

export interface UpdateWebhook {
	name?: string;
	url?: string;
	enabled?: boolean;
}

/** One destination's copy of one event, with what became of it. `last_error` is the
 * sanitized failure — it never carries the URL the send could not reach. */
export interface Delivery {
	id: string;
	webhook_id: string;
	event_id: string;
	event_kind: string;
	instance_id: string | null;
	status: 'pending' | 'delivered' | 'failed';
	attempts: number;
	last_error: string | null;
	created_at: string;
	updated_at: string;
}

export const webhookAdmin = {
	list: () => api.get<Page<Webhook>>('/admin/webhooks').then((page) => page.items),
	create: (body: CreateWebhook) => api.post<Webhook>('/admin/webhooks', body),
	update: (id: string, body: UpdateWebhook) =>
		api.patch<Webhook>(`/admin/webhooks/${encodeURIComponent(id)}`, body),
	remove: (id: string) => api.del<void>(`/admin/webhooks/${encodeURIComponent(id)}`),
	/** Sends one real notification down the real path, so what is verified is the address
	 * policy and the destination's own credential rather than a form validator. */
	test: (id: string) => api.post<Job>(`/admin/webhooks/${encodeURIComponent(id)}/test`),
	deliveries: () => api.get<Page<Delivery>>('/admin/webhooks/deliveries').then((page) => page.items)
};

export const keyAdmin = {
	/** Publishes a new derived-key generation and re-encrypts every stored secret under it
	 * (`10 §3.3`). Retrying after an interrupted run continues the same generation. */
	rotate: () => api.post<Job>('/admin/keys/rotate')
};

/** One diagnosed fact. `status` and `source` are closed sets the backend owns. */
export interface DiagnosticCheck {
	id: string;
	group: string;
	title: string;
	status: 'ok' | 'warn' | 'fail' | 'unknown';
	source: 'live' | 'startup' | 'job' | 'config';
	detail: string;
	/** Verbatim output from whatever was probed. Absent from the support bundle. */
	diagnostic?: string;
	remedy?: string;
	measured_at?: string;
}

export interface DiagnosticInstance {
	id: string;
	name: string;
	state: string;
	image: string;
	base_port: number;
	expected_ports: number[];
	bound_ports: number[];
	mods: number;
	restart_required: boolean;
	running: boolean;
	log_reader_attached: boolean;
	server_free_bytes: number | null;
	port_issue?: string;
}

export interface DiagnosticsReport {
	generated_at: string;
	build: { version: string; commit: string; built_at: string; modified: boolean; go: string };
	started_at: string;
	checks: DiagnosticCheck[];
	instances: DiagnosticInstance[];
	migrations: string[];
}

export const diagnostics = {
	read: () => api.get<DiagnosticsReport>('/admin/diagnostics'),
	/** The deep checks spawn containers, so they are a job rather than a request. */
	run: () => api.post<Job>('/admin/diagnostics/run'),
	/** A plain href: the browser downloads it with the session cookie it already has. */
	bundleUrl: () => '/api/v1/admin/diagnostics/bundle'
};
