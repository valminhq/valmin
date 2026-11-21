import type { Backup, WorldAttention } from '#lib/api/backups.js';

/** How long the daemon keeps player observations, and the longest range it serves. */
export const PLAYER_HISTORY_MS = 30 * 86_400_000;

/** What the catalogue itself says changed between two archives of one server. */
export interface WorldChange {
	older: Backup;
	newer: Backup;
	/** Newer world size minus older; null when either was never measured. */
	deltaBytes: number | null;
	/** Findings the newer archive has that the older did not. */
	newFindings: WorldAttention[];
	/** Archives taken strictly between the two, newest first. */
	between: Backup[];
}

/** Orders a pair by creation time and reads the world change from the loaded catalogue. */
export function worldChange(a: Backup, b: Backup, list: Backup[]): WorldChange {
	const [older, newer] = Date.parse(a.created_at) <= Date.parse(b.created_at) ? [a, b] : [b, a];
	const from = Date.parse(older.created_at);
	const to = Date.parse(newer.created_at);
	const known = new Set(older.world_attention.map((f) => f.code));
	return {
		older,
		newer,
		deltaBytes:
			older.world_bytes === null || newer.world_bytes === null
				? null
				: newer.world_bytes - older.world_bytes,
		newFindings: newer.world_attention.filter((f) => !known.has(f.code)),
		between: list.filter((x) => {
			const at = Date.parse(x.created_at);
			return at > from && at < to;
		})
	};
}

/** Whether the daemon still holds player observations for the whole of a non-empty [from, to). */
export function playersRetained(from: string, to: string, now: number): boolean {
	const span = Date.parse(to) - Date.parse(from);
	return span > 0 && span <= PLAYER_HISTORY_MS && Date.parse(from) >= now - PLAYER_HISTORY_MS;
}

/** The local calendar day of an instant, as the audit page's day filters take it. */
export function localDay(iso: string): string {
	const d = new Date(iso);
	const pad = (n: number) => String(n).padStart(2, '0');
	return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
