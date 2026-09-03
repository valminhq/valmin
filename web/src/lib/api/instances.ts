import { api } from './client';
import type { Job } from './types';

/** One line of `GET /instances/{id}/logs`. `↯` No `seq`: sequence numbers belong to the
 * panel's ring buffer, and these lines come from Docker. A client that spliced this into a
 * live console would be inventing a continuity neither side promised. */
export interface LogLine {
	ts: string;
	stream: 'stdout' | 'stderr';
	line: string;
}

/** `GET /instances/{id}/stats` — the one-shot read behind subscribe-then-fetch for a graph.
 * Every number is nullable: a stopped server has no resource usage, and zeros for it are the
 * same lie `cpu_pct: 0` would be on a first sample. */
export interface StatsReading {
	available: boolean;
	ts: string | null;
	cpu_pct: number | null;
	mem_bytes: number | null;
	mem_limit: number | null;
	mem_pct: number | null;
	/** `↯` Always null (E7, Q7). Render "unknown", never 0. */
	players: number | null;
}

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

/** `GET /instances/{id}/disk` — allocated bytes, what `du` reports. `↯` Split by category
 * because the question after "am I out of space" is "what can I safely delete": `server` is a
 * re-download, `backups` is prunable, `worlds` is gone for good (`02 §5`). */
export interface DiskUsage {
	total_bytes: number;
	server_bytes: number;
	worlds_bytes: number;
	logs_bytes: number;
	backups_bytes: number;
	measured_at: string;
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
	/** Installed once the server is provisioned and **before** it is started, so a mod that
	 * has anything to say about the world gets to say it before the world is written. The
	 * daemon resolves each one's dependencies and refuses the whole request if any closure
	 * cannot be computed. */
	mods?: Array<{ full_name: string; version: string }>;
}

/** A container carrying this panel's labels that no instance row claims (`08 §6.1`). M1
 * reports them; adoption is M5. `↯` It has no instance row, so it can never be shown on an
 * instance page — the list is the only place it can appear. */
export interface Orphan {
	container_id: string;
	name: string;
	instance_id: string;
	base_port: number;
	running: boolean;
}

/** Admin-only: the endpoint is gated on the never-grantable `panel.settings` (`09 §3.3`). */
export const orphans = () => api.get<Page<Orphan>>('/instances/orphans').then((p) => p.items);

interface Page<T> {
	items: T[];
	next_cursor: string | null;
}

export const instances = {
	list: () => api.get<Page<Instance>>('/instances').then((p) => p.items),
	get: (id: string) => api.get<Instance>(`/instances/${id}`),
	options: () => api.get<GameOptions>('/game/options'),
	logs: (id: string, tail = 500) =>
		api.get<Page<LogLine>>(`/instances/${id}/logs?tail=${tail}`).then((p) => p.items),
	stats: (id: string) => api.get<StatsReading>(`/instances/${id}/stats`),
	/** `↯` Not folded into stats(): that one is an in-memory sample, this one walks the
	 * instance's tree (~12 ms for a SteamCMD install). Read it on demand, never on a poll. */
	disk: (id: string) => api.get<DiskUsage>(`/instances/${id}/disk`),
	/** This instance's job history, newest first — where ADR-043's `registration
	 * unconfirmed` and `12 §3.4`'s `clean=false` live, and nowhere else. */
	jobs: (id: string, limit = 20) =>
		api.get<Page<Job>>(`/instances/${id}/jobs?limit=${limit}`).then((p) => p.items),

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
	consoleRead: 'console.read',
	statsRead: 'stats.read',
	modsList: 'mods.list',
	modsManage: 'mods.manage'
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
