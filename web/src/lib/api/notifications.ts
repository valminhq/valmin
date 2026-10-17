import { api } from './client';
import type { InboxKind } from './inbox';
import type { Job } from './types';

interface Page<T> {
	items: T[];
}

/** A notification destination. The URL is never returned: it is a bearer credential, so the
 * panel takes one and reports only what it is called. `instance_id` is the server it belongs to,
 * null when it is shared with every server. `catch_all` is whether the alerts no rule routes
 * reach it: an unexpected stop, a failed backup and a new game build. */
export interface Webhook {
	id: string;
	name: string;
	kind: 'discord' | 'generic';
	enabled: boolean;
	instance_id: string | null;
	catch_all: boolean;
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
	catch_all?: boolean;
}

export type DeliveryStatus = 'pending' | 'delivered' | 'failed';

/** One destination's copy of one event, with what became of it. `last_error` is the
 * sanitized failure — it never carries the URL the send could not reach. `rule_id` is the
 * alert rule that sent it: null when no rule did, and on rows older than the column. */
export interface Delivery {
	id: string;
	webhook_id: string;
	event_id: string;
	event_kind: string;
	instance_id: string | null;
	rule_id: string | null;
	status: DeliveryStatus;
	attempts: number;
	last_error: string | null;
	created_at: string;
	updated_at: string;
}

/** Narrows the delivery list on the daemon. An omitted field matches every row. */
export interface DeliveryFilter {
	webhook_id?: string;
	rule_id?: string;
	status?: DeliveryStatus;
}

export interface DeliveryPage {
	items: Delivery[];
	next_cursor: string | null;
}

/** An event the ValminEye plugin reports from inside the game. */
export type GameKind =
	| 'player_joined'
	| 'player_left'
	| 'boss_defeated'
	| 'boss_fight_started'
	| 'boss_fight_ended'
	| 'raid_started'
	| 'raid_ended';

/** What a rule can route: a condition the scan evaluates, or a one-off event. */
export type RuleKind = InboxKind | 'power_cut' | 'server_started' | 'server_stopped' | GameKind;

/** Which destinations hear about one kind of event, on one server or on every server, with the
 * kind's thresholds, its message and an optional quiet window. */
export interface AlertRule {
	id: string;
	instance_id: string | null;
	condition_kind: RuleKind;
	params: {
		crash_count?: number;
		crash_window_seconds?: number;
		stuck_after_seconds?: number;
		stale_factor?: number;
		/** Message fields the rule leaves out. */
		hidden_fields?: string[];
		/** IANA zone the rule's messages show times in; absent is UTC. */
		timezone?: string;
		/** The rule's message, with {field} placeholders; absent is the default. */
		template?: string;
		/** The Discord layout: absent puts the server and the event on one title line. */
		layout?: 'message' | 'plain';
		hide_link?: boolean;
		hide_time?: boolean;
		/** The card's colour as #RRGGBB; absent is the event's own. */
		color?: string;
	};
	quiet_start_minutes: number | null;
	quiet_end_minutes: number | null;
	quiet_timezone: string | null;
	enabled: boolean;
	webhook_ids: string[];
	created_at: string;
	updated_at: string;
}

/** `params` replaces the stored thresholds wholesale. The quiet fields travel together, in
 * minutes from midnight; an empty `quiet_timezone` clears the window. A server's routes make
 * every rule about that server, so `instance_id` is left out there. */
export interface CreateAlertRule {
	condition_kind: RuleKind;
	instance_id?: string | null;
	webhook_ids: string[];
	params?: AlertRule['params'];
	quiet_start_minutes?: number;
	quiet_end_minutes?: number;
	quiet_timezone?: string;
}

/** An omitted field is kept as it was. */
export interface UpdateAlertRule extends Partial<CreateAlertRule> {
	enabled?: boolean;
}

/** A rule as the editor holds it, tried on one webhook with the preview's sample values. */
export interface RuleTest {
	condition_kind: RuleKind;
	params: AlertRule['params'];
	webhook_id: string;
	fields: Record<string, string>;
}

/**
 * The webhook and alert rule endpoints of one scope: the panel's shared webhooks and every rule
 * through the admin routes, or one server's own webhooks and rules through its routes.
 */
export function notifications(instanceId?: string) {
	const base = instanceId ? `/instances/${encodeURIComponent(instanceId)}` : '/admin';
	const hook = (id: string) => `${base}/webhooks/${encodeURIComponent(id)}`;
	const rule = (id: string) => `${base}/alert-rules/${encodeURIComponent(id)}`;
	return {
		webhooks: {
			list: () => api.get<Page<Webhook>>(`${base}/webhooks`).then((page) => page.items),
			create: (body: CreateWebhook) => api.post<Webhook>(`${base}/webhooks`, body),
			update: (id: string, body: UpdateWebhook) => api.patch<Webhook>(hook(id), body),
			remove: (id: string) => api.del<void>(hook(id)),
			/** Sends one real notification down the real path, so what is verified is the address
			 * policy and the destination's own credential rather than a form validator. */
			test: (id: string) => api.post<Job>(`${hook(id)}/test`),
			/** One page of deliveries, newest first. Pass the same filter with the cursor it returned. */
			deliveries: (filter: DeliveryFilter = {}, cursor?: string, signal?: AbortSignal) => {
				const query = new URLSearchParams();
				for (const [key, value] of Object.entries({ ...filter, cursor }))
					if (value) query.set(key, value);
				return api.get<DeliveryPage>(`${base}/webhooks/deliveries?${query}`, signal);
			}
		},
		rules: {
			list: () => api.get<Page<AlertRule>>(`${base}/alert-rules`).then((page) => page.items),
			create: (body: CreateAlertRule) => api.post<AlertRule>(`${base}/alert-rules`, body),
			update: (id: string, body: UpdateAlertRule) => api.patch<AlertRule>(rule(id), body),
			remove: (id: string) => api.del<void>(rule(id)),
			/** Sends the message a rule would, over sample values, to one webhook it may use. */
			test: (body: RuleTest) => api.post<Job>(`${base}/alert-rules/test`, body)
		}
	};
}

export type NotificationsApi = ReturnType<typeof notifications>;
