import type { GameKind } from '$lib/api/admin';

/** One game event kind as the Game events page shows it. */
export interface GameEventKind {
	kind: GameKind;
	label: string;
	/** The placeholders its template can name, as the plugin sends them. */
	placeholders: string[];
	/** The message a rule without a template sends (delivery.gameEvents). */
	template: string;
	/** Placeholder values for the preview, from the plugin's documented examples. */
	sample: Record<string, string>;
}

const SERVER = 'Vikings';
const FIGHTERS = 'Alex (2 deaths), Bo';

export const GAME_EVENTS: GameEventKind[] = [
	{
		kind: 'player_joined',
		label: 'Player joined',
		placeholders: ['player', 'id', 'server'],
		template: '{player} joined',
		sample: { player: 'Alex', id: 'Steam_76561198000000000', server: SERVER }
	},
	{
		kind: 'player_left',
		label: 'Player left',
		placeholders: ['player', 'id', 'server'],
		template: '{player} left',
		sample: { player: 'Alex', id: 'Steam_76561198000000000', server: SERVER }
	},
	{
		kind: 'boss_defeated',
		label: 'Boss defeated for the first time',
		placeholders: ['boss', 'key', 'day', 'duration', 'wipes', 'players', 'server'],
		template: '{boss} was defeated for the first time on day {day}. Fighters: {players}',
		sample: {
			boss: 'Eikthyr',
			key: 'defeated_eikthyr',
			day: '12',
			duration: '95',
			wipes: '1',
			players: FIGHTERS,
			server: SERVER
		}
	},
	{
		kind: 'boss_fight_started',
		label: 'Boss fight started',
		placeholders: ['boss', 'key', 'first', 'day', 'players', 'server'],
		template: '{players} started a fight with {boss}',
		sample: {
			boss: 'The Elder',
			key: 'defeated_gdking',
			first: 'true',
			day: '31',
			players: 'Alex, Bo',
			server: SERVER
		}
	},
	{
		kind: 'boss_fight_ended',
		label: 'Boss fight ended',
		placeholders: ['boss', 'key', 'first', 'outcome', 'day', 'duration', 'players', 'server'],
		template: 'The fight with {boss} is over ({outcome}) after {duration} s. Fighters: {players}',
		sample: {
			boss: 'The Elder',
			key: 'defeated_gdking',
			first: 'true',
			outcome: 'wipe',
			day: '31',
			duration: '184',
			players: 'Alex (2 deaths)',
			server: SERVER
		}
	},
	{
		kind: 'raid_started',
		label: 'Raid started',
		placeholders: ['name', 'raid', 'players', 'server'],
		template: '{name}. Players in the raid: {players}',
		sample: { name: 'The ground is shaking', raid: 'army_eikthyr', players: 'Alex', server: SERVER }
	},
	{
		kind: 'raid_ended',
		label: 'Raid ended',
		placeholders: ['name', 'raid', 'players', 'server'],
		template: 'The raid is over: {name}',
		sample: { name: 'The ground is shaking', raid: 'army_eikthyr', players: 'Alex', server: SERVER }
	}
];

export const GAME_KINDS: GameKind[] = GAME_EVENTS.map((e) => e.kind);

/** Whether a rule kind is a game event, managed on the Game events page. */
export function isGameKind(kind: string): kind is GameKind {
	return (GAME_KINDS as string[]).includes(kind);
}

/** Replaces each {name} with its value; a name without one becomes empty, as the daemon does. */
export function expand(template: string, vars: Record<string, string>): string {
	return template.replace(/\{([a-z_]+)\}/g, (_, name: string) => vars[name] ?? '');
}
