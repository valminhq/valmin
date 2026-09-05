import { api } from './client';
import type { Job } from './types';

/** One line of `GET /instances/{id}/logs`. No `seq`: these come from Docker, while sequence
 * numbers belong to the panel's ring buffer, so the two cannot be spliced together. */
export interface LogLine {
	ts: string;
	stream: 'stdout' | 'stderr';
	line: string;
}

/** `GET /instances/{id}/stats`, the one-shot read behind subscribe-then-fetch for a graph.
 * Every number is nullable: a stopped server has no resource usage, and a zero would read as
 * one. */
export interface StatsReading {
	available: boolean;
	ts: string | null;
	cpu_pct: number | null;
	mem_bytes: number | null;
	mem_limit: number | null;
	mem_pct: number | null;
	/** Always null (E7, Q7). Render "unknown", never 0. */
	players: number | null;
}

/** An instances row as `GET /instances` serves it (`04 §2`). `password` is deliberately absent:
 * it has its own audited endpoint (`11 §9`), and a missing field cannot be rendered by
 * accident. */
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
	/** This boot's crossplay join code, read from the server's log. Null until the session
	 * logs one, and null again after a restart until the new session does. */
	crossplay_join_code: string | null;
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

/** `GET /game/options`, the measured launch vocabulary of `03 §1.3`. Served rather than
 * hardcoded here, since the frontend holds no Valheim knowledge (F2). */
export interface GameOptions {
	build: string;
	presets: string[];
	/** False: the preset list is confirmed but not exhaustive, and the UI says so. */
	presets_complete: boolean;
	modifier_keys: string[];
	/** False: the `.fwl`'s stored form is not proven to be the command-line grammar, so there is
	 * no value list to offer. */
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

/** `GET /instances/{id}/disk`, allocated bytes as `du` reports them. Split by category because
 * they differ in what losing them costs: `server` is a re-download, `backups` is prunable,
 * `worlds` is gone for good (`02 §5`). */
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
	/** Installed after provisioning and before the first start, so a mod affecting the world
	 * applies before the world is written. The daemon resolves each one's dependencies and
	 * refuses the whole request if any closure cannot be computed. */
	mods?: Array<{ full_name: string; version: string }>;
}

/** The launch fields `PATCH /instances/{id}` accepts from a settings screen. Absent means
 * unchanged (`11 §1.1`), so the form sends only what was touched. `world_name` is missing on
 * purpose: `-world` names the save file on disk, so a rename moves files rather than writing a
 * column (Q48). The resource fields the endpoint also takes belong to other capabilities. */
export interface PatchInstance {
	server_name?: string;
	password?: string;
	public?: boolean;
	crossplay?: boolean;
	preset?: string;
	modifiers?: Record<string, string>;
}

/** A container carrying this panel's labels that no instance row claims (`08 §6.1`). With no
 * row to belong to, the list is the only place it can appear. */
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
	/** Not folded into stats(): that one is an in-memory sample, this one walks the instance's
	 * tree. Read it on demand, never on a poll. */
	disk: (id: string) => api.get<DiskUsage>(`/instances/${id}/disk`),
	/** This instance's job history, newest first. The only place `registration unconfirmed`
	 * (ADR-043) and `clean=false` (`12 §3.4`) are reported. */
	jobs: (id: string, limit = 20) =>
		api.get<Page<Job>>(`/instances/${id}/jobs?limit=${limit}`).then((p) => p.items),

	// Every one of these returns a job, never the resource (ADR-028, `11 §3`). The job holds its
	// lock, so a second click is `409 job_in_progress` rather than a second run.
	create: (body: CreateInstance) => api.post<Job>('/instances', body),
	/** The exception: this writes columns, so it returns the row rather than a job. The container
	 * catches up on the next start, which rebuilds it if the row no longer describes it
	 * (ADR-118, ADR-121). */
	patch: (id: string, body: PatchInstance) => api.patch<Instance>(`/instances/${id}`, body),
	/** `POST /instances/{id}/worlds/import` (`11 §8.3`). Every part is sent under one field name,
	 * since the daemon keys on each part's filename; a zip of a whole save folder arrives the
	 * same way. `allow_backup_variant` carries the explicit choice `03 §4.1` rule 5 requires,
	 * which cannot be inferred from the bytes. */
	importWorld: (id: string, files: File[], allowBackupVariant: boolean) => {
		const form = new FormData();
		for (const file of files) form.append('file', file);
		return api.upload<Job>(
			`/instances/${id}/worlds/import?allow_backup_variant=${allowBackupVariant}`,
			form
		);
	},
	start: (id: string) => api.post<Job>(`/instances/${id}/start`),
	stop: (id: string) => api.post<Job>(`/instances/${id}/stop`),
	restart: (id: string) => api.post<Job>(`/instances/${id}/restart`),
	acknowledge: (id: string) => api.post<Instance>(`/instances/${id}/acknowledge`),
	remove: (id: string, keepWorlds: boolean) =>
		api.del<Job>(`/instances/${id}?keep_worlds=${keepWorlds}`)
};

/** The actions `09 §3` names, as the strings `allowed_actions` carries. The UI renders from
 * these and never from a role (F3), typed so a component cannot invent one that never
 * matches. */
export const actions = {
	view: 'instance.view',
	start: 'instance.start',
	stop: 'instance.stop',
	restart: 'instance.restart',
	create: 'instance.create',
	remove: 'instance.delete',
	settings: 'instance.settings',
	worldImport: 'world.import',
	consoleRead: 'console.read',
	statsRead: 'stats.read',
	modsList: 'mods.list',
	modsManage: 'mods.manage',
	configRead: 'config.read',
	configEdit: 'config.edit',
	configRaw: 'config.raw'
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
