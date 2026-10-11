import type { ManifestConfig, ManifestLaunch } from '$lib/api/manifest';
import type { InstalledMod } from '$lib/api/mods';

/** How one item on the first server relates to the same item on the second. */
export type Change = 'same' | 'changed' | 'only-a' | 'only-b';

/** One key present on either server, with each side's item or null where it is absent. */
export interface Row<T> {
	key: string;
	a: T | null;
	b: T | null;
	change: Change;
}

/** One setting, both values already worded for display. */
export interface SettingRow {
	key: string;
	label: string;
	a: string;
	b: string;
	change: Change;
}

/** The part of a server the settings comparison reads. */
export interface Settings {
	build?: string;
	launch: ManifestLaunch;
}

export type ModState = Pick<InstalledMod, 'full_name' | 'version' | 'enabled' | 'source'> &
	Partial<Pick<InstalledMod, 'locked' | 'side' | 'installed_as'>>;

const notSet = 'Not set';

/** Launch fields in display order, labelled as the settings and backups screens label their
 * controls. */
const labels: Record<string, string> = {
	server_name: 'Server name',
	world_name: 'World name',
	public: 'List publicly',
	crossplay: 'Crossplay',
	preset: 'World preset',
	modifiers: 'Modifier',
	extra_args: 'Extra arguments',
	mem_limit_mb: 'Memory limit (MB)',
	cpu_limit: 'CPU limit (cores)',
	backup_keep_cold: 'Consistent backups to keep',
	backup_keep_hot: 'Best-effort backups to keep',
	backup_on_restart: 'Back up when this server restarts'
};

function display(value: unknown): string {
	if (value === null || value === undefined || value === '') return notSet;
	if (typeof value === 'boolean') return value ? 'Yes' : 'No';
	return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

function union(...lists: string[][]): string[] {
	return [...new Set(lists.flat())];
}

function setting(key: string, label: string, a: string, b: string): SettingRow {
	return { key, label, a, b, change: a === b ? 'same' : 'changed' };
}

/** Pairs two lists by key into one row per key, sorted by key. */
export function pair<T>(
	a: T[],
	b: T[],
	key: (item: T) => string,
	same: (x: T, y: T) => boolean
): Row<T>[] {
	const left = new Map(a.map((item) => [key(item), item]));
	const right = new Map(b.map((item) => [key(item), item]));
	return union([...left.keys()], [...right.keys()])
		.sort((x, y) => x.localeCompare(y))
		.map((k) => {
			const x = left.get(k) ?? null;
			const y = right.get(k) ?? null;
			const change: Change = !y ? 'only-a' : !x ? 'only-b' : same(x, y) ? 'same' : 'changed';
			return { key: k, a: x, b: y, change };
		});
}

/** The game build first, then every launch field, with one row per modifier key. */
export function compareSettings(a: Settings, b: Settings): SettingRow[] {
	const rows = [setting('game_build', 'Game build', a.build || 'Unknown', b.build || 'Unknown')];
	const left = new Map<string, unknown>(Object.entries(a.launch));
	const right = new Map<string, unknown>(Object.entries(b.launch));
	for (const key of union(Object.keys(labels), [...left.keys()], [...right.keys()])) {
		if (key !== 'modifiers') {
			rows.push(setting(key, labels[key] ?? key, display(left.get(key)), display(right.get(key))));
			continue;
		}
		const ma = a.launch.modifiers ?? {};
		const mb = b.launch.modifiers ?? {};
		for (const name of union(Object.keys(ma), Object.keys(mb)).sort()) {
			rows.push(
				setting(
					`modifiers.${name}`,
					`${labels.modifiers}: ${name}`,
					display(ma[name]),
					display(mb[name])
				)
			);
		}
	}
	return rows;
}

/** Installed mods by package and their recorded recovery state. */
export function compareMods(a: ModState[], b: ModState[]): Row<ModState>[] {
	return pair(
		a,
		b,
		(mod) => mod.full_name,
		(x, y) =>
			x.version === y.version &&
			x.source === y.source &&
			x.enabled === y.enabled &&
			x.locked === y.locked &&
			x.side === y.side &&
			x.installed_as === y.installed_as
	);
}

/** Config files by name; a file differs when its content does. */
export function compareConfigs(a: ManifestConfig[], b: ManifestConfig[]): Row<ManifestConfig>[] {
	return pair(
		a,
		b,
		(config) => config.file,
		(x, y) => x.content === y.content
	);
}

/** What differs about one mod row, worded with the two server names. */
export function modChanges(row: Row<ModState>, aName: string, bName: string): string[] {
	if (!row.b) return [`Only on ${aName}`];
	if (!row.a) return [`Only on ${bName}`];
	const out: string[] = [];
	if (row.a.version !== row.b.version) out.push('Different version');
	if (row.a.source !== row.b.source) out.push('Different registry');
	if (row.a.enabled !== row.b.enabled) {
		out.push(`Enabled only on ${row.a.enabled ? aName : bName}`);
	}
	if (row.a.locked !== row.b.locked) out.push('Different version lock');
	if (row.a.side !== row.b.side) out.push('Different client tag');
	if (row.a.installed_as !== row.b.installed_as) out.push('Different install reason');
	return out;
}
