import type { AuditEntry, AuditOutcome } from '$lib/api/admin';
import type { BadgeVariant } from '$lib/components/ui/badge';

type Detail = Record<string, unknown>;

/** One field of a recorded change, with values already rendered as text. */
export interface ChangeRow {
	field: string;
	from: string;
	to: string;
	secret: boolean;
}

const LABELS: Record<string, string> = {
	'instances.create': 'Server created',
	'instances.start': 'Server started',
	'instances.stop': 'Server stopped',
	'instances.restart': 'Server restarted',
	'instances.delete': 'Server deleted',
	'instances.game.update': 'Game updated',
	'instances.backups.create': 'Backup created',
	'instances.backups.restore': 'Backup restored',
	'instances.backups.delete': 'Backup deleted',
	'instances.setups.save': 'Setup saved',
	'instances.setups.restore': 'Setup restored',
	'instances.setups.delete': 'Setup deleted',
	'instances.worlds.import': 'World imported',
	'instances.worlds.restore': 'World restored',
	'instances.worlds.delete': 'World deleted',
	'instances.operation.resume': 'Operation resumed',
	'instances.operation.abandon': 'Operation abandoned',
	'jobs.cancel': 'Job cancelled',
	'instances.mods.install': 'Mod installed',
	'instances.mods.update': 'Mods updated',
	'instances.mods.uninstall': 'Mods uninstalled',
	'instances.mods.enable': 'Mod enabled',
	'instances.mods.disable': 'Mod disabled',
	'instances.mods.lock': 'Mod version locked',
	'instances.mods.unlock': 'Mod version unlocked',
	'instances.settings.update': 'Settings changed',
	'instances.configs.write': 'Config file edited',
	'instances.configs.delete': 'Config file deleted',
	'instances.commands.send': 'Command sent',
	'schedules.create': 'Schedule created',
	'schedules.update': 'Schedule changed',
	'schedules.delete': 'Schedule deleted',
	'users.password.change': 'Users password changed'
};

const OUTCOMES: Record<AuditOutcome, { label: string; variant: BadgeVariant }> = {
	requested: { label: 'In progress', variant: 'outline' },
	succeeded: { label: 'Completed', variant: 'secondary' },
	failed: { label: 'Failed', variant: 'destructive' },
	cancelled: { label: 'Cancelled', variant: 'outline' }
};

const spaced = (name: string) => name.replaceAll('_', ' ');

/** A table entry by its own key: an action named like an Object method finds nothing. */
const own = <T>(table: Record<string, T>, key: string): T | undefined =>
	Object.hasOwn(table, key) ? table[key] : undefined;

const upperFirst = (text: string) => text.charAt(0).toUpperCase() + text.slice(1);

/** A readable name for an action: the known label, else the dotted name in words. */
export function label(action: string): string {
	return own(LABELS, action) ?? upperFirst(spaced(action.replaceAll('.', ' ')));
}

/** The badge text for an outcome, or null when the entry carries none. */
export function outcomeLabel(outcome: AuditOutcome | null): string | null {
	return outcome ? (own(OUTCOMES, outcome)?.label ?? outcome) : null;
}

export function outcomeVariant(outcome: AuditOutcome | null): BadgeVariant {
	return (outcome && own(OUTCOMES, outcome)?.variant) || 'outline';
}

type Subject = Pick<AuditEntry, 'actor' | 'user_id'>;

/** The actor as the trail names them: the name at the time, or what became of them. */
export function actorName(entry: Subject): string {
	return entry.actor ?? (entry.user_id ? 'deleted user' : 'the panel');
}

type Target = Pick<AuditEntry, 'instance' | 'instance_id'>;

/** The server's name at the time, "deleted server" once it is gone, null for no server. */
export function serverName(entry: Target): string | null {
	return entry.instance ?? (entry.instance_id ? 'deleted server' : null);
}

/** Parses an entry's detail as an object; null when it is anything else. */
function parse(raw: string | null): Detail | null {
	if (!raw) return {};
	try {
		const value: unknown = JSON.parse(raw);
		return isObject(value) ? value : null;
	} catch {
		return null;
	}
}

function isObject(value: unknown): value is Detail {
	return value !== null && typeof value === 'object' && !Array.isArray(value);
}

const text = (value: unknown): string =>
	typeof value === 'string' ? value : typeof value === 'number' ? String(value) : '';

const objects = (value: unknown): Detail[] => (Array.isArray(value) ? value.filter(isObject) : []);

/** A value as it reads in a change: absent and empty are said so rather than left blank. */
function show(value: unknown): string {
	if (value === undefined || value === null) return '(none)';
	if (value === '') return '(empty)';
	return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

function rowOf(change: Detail): ChangeRow {
	const secret = change.secret === true;
	return {
		field: text(change.field) || '(unnamed)',
		from: secret ? '' : show(change.from),
		to: secret ? '' : show(change.to),
		secret
	};
}

/** The structured changes an entry recorded, empty when its detail carries none. */
export function changes(detail: string | null): ChangeRow[] {
	const d = parse(detail);
	if (!d) return [];
	if (Array.isArray(d.changes)) return objects(d.changes).map(rowOf);
	if (Array.isArray(d.packages)) {
		return objects(d.packages).map((p) => rowOf({ field: p.full_name, from: p.from, to: p.to }));
	}
	if (text(d.full_name) && d.to !== undefined) {
		return [rowOf({ field: d.full_name, from: d.from, to: d.to })];
	}
	return [];
}

/** The first two items, then a count of the rest. */
function listed(items: string[]): string {
	if (items.length <= 2) return items.join(' and ');
	return `${items[0]}, ${items[1]} and ${items.length - 2} more`;
}

function changePhrase(row: ChangeRow): string {
	return row.secret ? `the ${row.field}` : `${row.field} from ${row.from} to ${row.to}`;
}

function configPhrase(row: ChangeRow): string {
	return row.secret ? `${row.field} (value hidden)` : `${row.field} ${row.from} → ${row.to}`;
}

const clip = (raw: string, max = 120) => (raw.length > max ? `${raw.slice(0, max)}…` : raw);

/**
 * Builds the phrase that follows the actor. `server` is the name to say for the server the
 * entry is about, empty when there is none; `on` is the same as a trailing " on X".
 */
type Phrase = (d: Detail, server: string, on: string) => string;

const PHRASES: Record<string, Phrase> = {
	'instances.create': (d) => {
		const name = text(d.name) || 'a server';
		return d.source === 'manifest' ? `created ${name} from a manifest` : `created ${name}`;
	},
	'instances.start': (_, s) => `started ${s || 'a server'}`,
	'instances.stop': (_, s) => `stopped ${s || 'a server'}`,
	'instances.restart': (_, s) => `restarted ${s || 'a server'}`,
	'instances.delete': (d, s) => {
		const worlds =
			d.keep_worlds === true
				? ' and kept its worlds'
				: d.keep_worlds === false
					? ' and its worlds'
					: '';
		return `deleted ${s || 'a server'}${worlds}`;
	},
	'instances.game.update': (_, __, on) => `updated the game${on}`,
	'instances.backups.create': (_, __, on) => `created a backup${on}`,
	'instances.backups.restore': (_, __, on) => `restored a backup${on}`,
	'instances.backups.delete': (_, __, on) => `deleted a backup${on}`,
	'instances.setups.save': (d, _, on) => `saved setup ${text(d.name) || '(unnamed)'}${on}`,
	'instances.setups.restore': (d, _, on) => `restored setup ${text(d.name) || '(unnamed)'}${on}`,
	'instances.setups.delete': (d, _, on) => `deleted setup ${text(d.name) || '(unnamed)'}${on}`,
	'instances.worlds.import': (d, _, on) => `imported world ${text(d.world) || '(unnamed)'}${on}`,
	'instances.worlds.restore': (d, _, on) => `restored world ${text(d.world) || '(unnamed)'}${on}`,
	'instances.worlds.delete': (d, _, on) => `deleted world ${text(d.world) || '(unnamed)'}${on}`,
	'instances.operation.resume': (_, __, on) => `resumed an interrupted operation${on}`,
	'instances.operation.abandon': (_, __, on) => `abandoned an interrupted operation${on}`,
	'jobs.cancel': (d, _, on) => `cancelled a ${spaced(text(d.kind)) || 'background'} job${on}`,
	'instances.mods.install': (d, _, on) => {
		const name = text(d.full_name) || 'a mod';
		const from = text(d.from);
		const to = text(d.to);
		if (from && to) return `changed ${name} from ${from} to ${to}${on}`;
		return `installed ${name}${to ? ` ${to}` : ''}${on}`;
	},
	'instances.mods.update': (d, _, on) => {
		const packages = objects(d.packages);
		if (packages.length !== 1) {
			return `updated ${packages.length > 1 ? `${packages.length} mods` : 'mods'}${on}`;
		}
		const [p] = packages;
		const name = text(p.full_name) || 'a mod';
		return text(p.from) && text(p.to)
			? `updated ${name} from ${text(p.from)} to ${text(p.to)}${on}`
			: `updated ${name}${on}`;
	},
	'instances.mods.uninstall': (d, _, on) => {
		const names = Array.isArray(d.full_names) ? d.full_names.map(text).filter(Boolean) : [];
		return names.length === 1
			? `uninstalled ${names[0]}${on}`
			: `uninstalled ${names.length > 1 ? `${names.length} mods` : 'mods'}${on}`;
	},
	'instances.mods.enable': (d, _, on) => `enabled ${text(d.full_name) || 'a mod'}${on}`,
	'instances.mods.disable': (d, _, on) => `disabled ${text(d.full_name) || 'a mod'}${on}`,
	'instances.mods.lock': (d, _, on) => {
		const version = text(d.version);
		return `locked ${text(d.full_name) || 'a mod'}${version ? ` at ${version}` : ''}${on}`;
	},
	'instances.mods.unlock': (d, _, on) => `unlocked ${text(d.full_name) || 'a mod'}${on}`,
	'instances.settings.update': (d, _, on) => {
		const rows = objects(d.changes).map(rowOf);
		return rows.length ? `changed ${listed(rows.map(changePhrase))}${on}` : `changed settings${on}`;
	},
	'instances.configs.write': (d, _, on) => {
		const file = text(d.file) || 'a config file';
		const rows = objects(d.changes).map(rowOf);
		return rows.length
			? `edited ${file}${on}: ${listed(rows.map(configPhrase))}`
			: `edited ${file}${on}`;
	},
	'instances.configs.delete': (d, _, on) => `deleted ${text(d.file) || 'a config file'}${on}`,
	'instances.commands.send': (d, _, on) => {
		const command = text(d.command);
		return command ? `sent command '${command}'${on}` : `sent a command${on}`;
	},
	'schedules.create': (d, _, on) => {
		const cron = text(d.cron);
		return `created a ${spaced(text(d.kind)) || 'maintenance'} schedule${cron ? ` (${cron})` : ''}${
			d.enabled === false ? ', disabled' : ''
		}${on}`;
	},
	'schedules.update': (d, _, on) => {
		const rows = objects(d.changes).map(rowOf);
		return `changed the ${spaced(text(d.kind)) || 'maintenance'} schedule${on}${
			rows.length ? `: ${listed(rows.map(changePhrase))}` : ''
		}`;
	},
	'schedules.delete': (d, _, on) => {
		const cron = text(d.cron);
		return `deleted the ${spaced(text(d.kind)) || 'maintenance'} schedule${cron ? ` (${cron})` : ''}${on}`;
	}
};

/**
 * One sentence for an entry: who did what to which server. An action this file does not know,
 * or a detail that is not the JSON object the action records, reads as the action's name and
 * the detail as stored.
 */
export function describe(entry: AuditEntry): string {
	const actor = upperFirst(actorName(entry));
	const phrase = own(PHRASES, entry.action);
	const detail = phrase ? parse(entry.detail) : null;
	if (phrase && detail) {
		const server = entry.instance ?? (entry.instance_id ? 'a deleted server' : '');
		return `${actor} ${phrase(detail, server, server ? ` on ${server}` : '')}`;
	}
	const raw = entry.detail ? `: ${clip(entry.detail)}` : '';
	return `${actor}: ${label(entry.action)}${raw}`;
}

const DAY = /^\d{4}-\d{2}-\d{2}$/;

function startOf(day: string): number {
	if (!DAY.test(day)) return NaN;
	const [year, month, date] = day.split('-').map(Number);
	const start = new Date(year, month - 1, date);
	return start.getFullYear() === year && start.getMonth() === month - 1 && start.getDate() === date
		? start.getTime()
		: NaN;
}

/**
 * The half-open window covering local days from `from` to `to` inclusive, as the daemon takes
 * it. An empty or malformed day leaves that end open.
 */
export function dayRange(from: string, to: string): { since?: string; until?: string } {
	const start = startOf(from);
	const end = startOf(to);
	const iso = (ms: number) => new Date(ms).toISOString().replace('.000Z', 'Z');
	const nextDay = (day: string) => {
		const [year, month, date] = day.split('-').map(Number);
		return new Date(year, month - 1, date + 1).getTime();
	};
	return {
		since: Number.isNaN(start) ? undefined : iso(start),
		until: Number.isNaN(end) ? undefined : iso(nextDay(to))
	};
}
