/** What an action lets a person do: a verb and the thing it acts on. */
interface Phrase {
	verb: string;
	object: string;
}

/** Every action a grant can carry, in the order a sentence lists them. */
const PHRASES: Record<string, Phrase> = {
	'instance.view': { verb: 'view', object: 'the server' },
	'console.read': { verb: 'view', object: 'the console' },
	'stats.read': { verb: 'view', object: 'stats' },
	'backups.list': { verb: 'view', object: 'backups' },
	'mods.list': { verb: 'view', object: 'mods' },
	'config.read': { verb: 'view', object: 'configuration' },
	'instance.start': { verb: 'start', object: 'the server' },
	'instance.stop': { verb: 'stop', object: 'the server' },
	'instance.restart': { verb: 'restart', object: 'the server' },
	'backups.create': { verb: 'create', object: 'backups' },
	'backups.download': { verb: 'download', object: 'backups' },
	'backups.restore': { verb: 'restore', object: 'backups' },
	'players.manage': { verb: 'manage', object: 'players' },
	'mods.manage': { verb: 'manage', object: 'mods' },
	'commands.send': { verb: 'send', object: 'console commands' },
	'config.edit': { verb: 'edit', object: 'configuration' },
	'config.raw': { verb: 'edit', object: 'raw configuration files' },
	'world.import': { verb: 'import', object: 'worlds' },
	'instance.settings': { verb: 'change', object: 'server settings' }
};

/** The phrase for an action; a name this table does not know becomes its own words. */
function phrase(action: string): Phrase {
	if (Object.hasOwn(PHRASES, action)) return PHRASES[action];
	return { verb: action.replaceAll(/[._]+/g, ' ').trim().toLowerCase(), object: '' };
}

/** A lowercase verb phrase that reads inside a sentence, such as "restart the server". */
export function actionLabel(action: string): string {
	const { verb, object } = phrase(action);
	return object ? `${verb} ${object}` : verb;
}

/** The text with its first letter upper-cased. */
export function capitalise(text: string): string {
	return text.charAt(0).toUpperCase() + text.slice(1);
}

/** Joins items with the separator and a final "and". Two items joined by commas need no
 * separator; two joined by semicolons keep it. */
function list(items: string[], separator = ', '): string {
	if (items.length < 2) return items.join('');
	if (items.length === 2 && separator === ', ') return items.join(' and ');
	return `${items.slice(0, -1).join(separator)}${separator}and ${items[items.length - 1]}`;
}

/**
 * One deterministic sentence for what a set of actions allows. Neighbouring actions that share
 * a verb or an object fold into one clause, so the read-only set reads as "view the server,
 * console, stats, backups, mods, and configuration".
 */
export function describeAccess(actions: string[]): string {
	const held = new Set(actions);
	// Raw editing includes form editing, so the narrower phrase would only repeat it.
	if (held.has('config.raw')) held.delete('config.edit');
	const known = Object.keys(PHRASES).filter((action) => held.has(action));
	const unknown = [...held].filter((action) => !Object.hasOwn(PHRASES, action)).sort();

	const clauses: { verbs: string[]; objects: string[] }[] = [];
	for (const { verb, object } of [...known, ...unknown].map(phrase)) {
		const last = clauses[clauses.length - 1];
		if (last && last.verbs.length === 1 && last.verbs[0] === verb) last.objects.push(object);
		else if (last && last.objects.length === 1 && last.objects[0] === object) last.verbs.push(verb);
		else clauses.push({ verbs: [verb], objects: [object] });
	}

	const texts = clauses.map(({ verbs, objects }) =>
		[list(verbs), list(objects.map((object, i) => (i ? object.replace(/^the /, '') : object)))]
			.filter(Boolean)
			.join(' ')
	);
	if (texts.length === 0) return 'Has no access.';
	return `Can ${list(texts, texts.some((text) => text.includes(',')) ? '; ' : ', ')}.`;
}
