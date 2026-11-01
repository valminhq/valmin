import { api } from './client';
import type { Job } from './types';

/**
 * One archive in the catalogue (`04 §3`). No path: the daemon keeps a filesystem path out of
 * every response (`11 §8.3`), so an archive is addressed by id and downloaded by id.
 */
export interface Backup {
	id: string;
	instance_id: string;
	size_bytes: number;
	sha256: string;
	world_name: string;
	/** Why the archive exists — `manual`, `scheduled`, `pre_restore`, `pre_update`,
	 * `pre_import`. Rendered as the daemon spells it; the SPA invents no trigger of its own. */
	trigger: string;
	/** False for a hot copy: the world was live while it was read, so it is best-effort and
	 * must never be presented as the safe one (B12). */
	consistent: boolean;
	created_at: string;
	filename: string;
	/** True when retention would remove this archive on its next run. The daemon decides it
	 * over the whole catalogue — a count kept here would be a second copy of one policy. */
	prunes_next: boolean;
	/** Size of the world inside the archive; null when it was never measured. */
	world_bytes: number | null;
	/** What the world check asks the operator to look at; empty when nothing. The daemon writes
	 * the messages, so the SPA holds no knowledge of world files. */
	world_attention: WorldAttention[];
}

/** One finding of the world check. A finding flags an archive; it never refuses one. */
export interface WorldAttention {
	code: string;
	message: string;
}

export interface Page<T> {
	items: T[];
	next_cursor: string | null;
}

/**
 * How an archive is taken. `quiesced` stops the server, waits for the save to finish and
 * archives a world nothing is writing to; `hot` copies a running server's world and is
 * best-effort (`12 §3.2`, B12). Absent means quiesced on the daemon too — the safe archive is
 * what someone who did not choose gets.
 */
export type BackupMode = 'quiesced' | 'hot';

export const backups = {
	/** Returns the page, cursor included: retention can hold more archives than one page, and
	 * an older recovery point that the panel cannot reach is a recovery point the operator does
	 * not have (`11 §4`). */
	list: (instanceId: string, cursor: string | null = null, limit = 50) => {
		const params = new URLSearchParams({ limit: String(limit) });
		if (cursor) params.set('cursor', cursor);
		return api.get<Page<Backup>>(`/instances/${instanceId}/backups?${params}`);
	},
	/** Returns a job, never the archive (ADR-028): a quiesced backup stops a server and can
	 * run for minutes. */
	create: (instanceId: string, mode: BackupMode) =>
		api.post<Job>(`/instances/${instanceId}/backups?mode=${mode}`),
	restore: (instanceId: string, backupId: string) =>
		api.post<Job>(`/instances/${instanceId}/backups/${backupId}/restore`),
	remove: (instanceId: string, backupId: string) =>
		api.del<void>(`/instances/${instanceId}/backups/${backupId}`),
	/** Accepts a flagged archive's world after the operator has checked it. */
	acknowledge: (instanceId: string, backupId: string) =>
		api.post<void>(`/instances/${instanceId}/backups/${backupId}/acknowledge`),
	/** A plain link rather than a fetch: the archive is served on the streaming router with no
	 * write deadline (`11 §8.1`), and the browser's own download handles a multi-gigabyte body
	 * without the SPA holding it in memory. */
	downloadUrl: (instanceId: string, backupId: string) =>
		`/api/v1/instances/${instanceId}/backups/${backupId}/download`
};
