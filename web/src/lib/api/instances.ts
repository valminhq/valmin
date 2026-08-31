import { api } from './client';
import type { Job } from './types';

/** An instances row as `GET /instances` serves it (`04 §2`). `password` is deliberately not
 * here: `11 §9` gives it its own audited endpoint, and a field that does not exist cannot
 * be rendered by accident. */
export interface Instance {
	id: string;
	name: string;
	state: string;
	base_port: number;
	server_name: string;
	world_name: string;
	public: boolean;
	crossplay: boolean;
	crossplay_instance_id: string;
	preset?: string;
	modifiers?: string;
	extra_args?: string;
	modded: boolean;
	restart_required: boolean;
	mem_limit_mb: number;
	cpu_limit?: number;
	game_build_id?: string;
	created_at: string;
	updated_at: string;
}

/** `GET /game/options` — the measured launch vocabulary of `03 §1.3`, served rather than
 * hardcoded here, because the frontend holds no Valheim knowledge (F2, `02 §2.1`). */
export interface GameOptions {
	build: string;
	presets: string[];
	/** `↯` False. Black-box probing confirms values it is given; it cannot enumerate the ones
	 * nobody tried, and the UI has to say so rather than present a probe as a list. */
	presets_complete: boolean;
	modifier_keys: string[];
	/** `↯` False. The `.fwl`'s stored form is not proven to be the command-line grammar, so
	 * there is no value list to offer and none is invented. */
	modifier_values_measured: boolean;
	save_defaults: {
		save_interval_seconds: number;
		backups: number;
		backup_short_seconds: number;
		backup_long_seconds: number;
	};
	crossplay_untested: string[];
	min_password_length: number;
}

export interface CreateInstance {
	name: string;
	server_name: string;
	world_name: string;
	password: string;
	public: boolean;
	crossplay: boolean;
	preset?: string;
	modifiers?: Record<string, string>;
	mem_limit_mb?: number;
	start_after_provision?: boolean;
}

interface Page<T> {
	items: T[];
	next_cursor: string | null;
}

export const instances = {
	list: () => api.get<Page<Instance>>('/instances').then((p) => p.items),
	get: (id: string) => api.get<Instance>(`/instances/${id}`),
	options: () => api.get<GameOptions>('/game/options'),

	// `↯` Every one of these returns a job, never the resource (ADR-028, `11 §3`). Reaching a
	// job means its lock is held; a second click is `409 job_in_progress`, which is also why
	// there are no idempotency keys anywhere in this API.
	create: (body: CreateInstance) => api.post<Job>('/instances', body),
	start: (id: string) => api.post<Job>(`/instances/${id}/start`),
	stop: (id: string) => api.post<Job>(`/instances/${id}/stop`),
	restart: (id: string) => api.post<Job>(`/instances/${id}/restart`),
	acknowledge: (id: string) => api.post<Instance>(`/instances/${id}/acknowledge`),
	remove: (id: string, keepWorlds: boolean) =>
		api.del<Job>(`/instances/${id}?keep_worlds=${keepWorlds}`)
};

/** The actions `09 §3` names, as the strings `allowed_actions` carries. `↯` The UI renders
 * from these and never from a role (F3) — and they are typed so a component cannot invent
 * one that silently never matches. */
export const actions = {
	view: 'instance.view',
	start: 'instance.start',
	stop: 'instance.stop',
	restart: 'instance.restart',
	create: 'instance.create',
	remove: 'instance.delete',
	consoleRead: 'console.read'
} as const;

/** States in which the instance is mid-transition, so the buttons wait rather than race
 * (`12 §2.1`). */
const TRANSIENT = new Set([
	'created',
	'provisioning',
	'starting',
	'stopping',
	'backing_up',
	'restoring',
	'updating',
	'deleting'
]);

export function isTransient(state: string): boolean {
	return TRANSIENT.has(state);
}
