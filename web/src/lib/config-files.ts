import type { ConfigFile, ConfigRevision } from '#lib/api/configs.js';

/** Whether the daemon serves settings for this file. Only a `.cfg` has them; any other config
 * file is edited as text. */
export function hasSettings(file: string): boolean {
	return file.endsWith('.cfg');
}

/**
 * The files whose name or plugin name contains the query, ignoring case and surrounding
 * whitespace. A blank query keeps every file, and the order given is the order returned.
 */
export function filterConfigs(files: ConfigFile[], query: string): ConfigFile[] {
	const needle = query.trim().toLowerCase();
	if (!needle) return files;
	return files.filter(
		(f) => f.file.toLowerCase().includes(needle) || f.plugin.toLowerCase().includes(needle)
	);
}

/** What produced a kept version, in words. */
export function revisionLabel(rev: ConfigRevision): string {
	switch (rev.source) {
		case 'found':
			return 'As first found';
		case 'outside':
			return 'Changed outside the panel';
		case 'edit':
			return rev.user ? `Saved by ${rev.user}` : 'Saved in the panel';
		case 'setup':
			return 'Written by a setup restore or import';
		case 'mod':
			return rev.from_version
				? `${rev.mod} ${rev.from_version} → ${rev.to_version}`
				: `Installed with ${rev.mod} ${rev.to_version}`;
	}
}

/**
 * The newest mod-written version when the version `id` is older than it, so loading `id` may
 * give the mod a file it no longer expects; null otherwise. `history` is newest first.
 */
export function newerModVersion(history: ConfigRevision[], id: string): ConfigRevision | null {
	const at = history.findIndex((rev) => rev.id === id);
	const mod = history.findIndex((rev) => rev.source === 'mod');
	return at >= 0 && mod >= 0 && at > mod ? history[mod] : null;
}
