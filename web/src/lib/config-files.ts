import type { ConfigFile } from '$lib/api/configs';

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
