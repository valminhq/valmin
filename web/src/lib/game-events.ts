import type { GameKind } from '$lib/api/notifications';
import type { NotifyEvent } from '$lib/message';

/** A ValminEye event a rule can send. Samples are the plugin's documented examples. */
export type GameEvent = NotifyEvent<GameKind, 'Players' | 'Bosses' | 'Raids'>;

const FIGHTERS = 'Alex (2 deaths), Bo';

export const GAME_EVENTS: GameEvent[] = [
	{
		kind: 'player_joined',
		group: 'Players',
		label: 'Player joined',
		when: 'A player’s character enters the world.',
		headline: 'Player joined',
		accent: '#2ECC71',
		template: '{player} joined',
		placeholders: ['player', 'id', 'server', 'event'],
		sample: { player: 'Alex', id: 'Steam_76561198000000000' }
	},
	{
		kind: 'player_left',
		group: 'Players',
		label: 'Player left',
		when: 'A player quits, loses connection, or is kicked.',
		headline: 'Player left',
		accent: '#95A5A6',
		template: '{player} left',
		placeholders: ['player', 'id', 'server', 'event'],
		sample: { player: 'Alex', id: 'Steam_76561198000000000' }
	},
	{
		kind: 'boss_fight_started',
		group: 'Bosses',
		label: 'Fight started',
		when: 'A boss notices players. By default only bosses not yet defeated in this world.',
		headline: 'Boss fight started',
		accent: '#E67E22',
		template: '{players} started a fight with {boss}',
		placeholders: ['boss', 'key', 'first', 'day', 'players', 'server', 'event'],
		sample: {
			boss: 'The Elder',
			key: 'defeated_gdking',
			first: 'true',
			day: '31',
			players: 'Alex, Bo'
		}
	},
	{
		kind: 'boss_fight_ended',
		group: 'Bosses',
		label: 'Fight ended',
		when: 'The boss dies, everyone dies or runs, or the boss disappears.',
		headline: 'Boss fight ended',
		accent: '#5865F2',
		template: 'The fight with {boss} is over ({outcome}) after {duration} s. Fighters: {players}',
		placeholders: [
			'boss',
			'key',
			'first',
			'outcome',
			'day',
			'duration',
			'players',
			'server',
			'event'
		],
		sample: {
			boss: 'The Elder',
			key: 'defeated_gdking',
			first: 'true',
			outcome: 'wipe',
			day: '31',
			duration: '184',
			players: 'Alex (2 deaths)'
		}
	},
	{
		kind: 'boss_defeated',
		group: 'Bosses',
		label: 'First defeat',
		when: 'A boss is defeated for the first time in this world.',
		headline: 'Boss defeated',
		accent: '#F1C40F',
		template: '{boss} was defeated for the first time on day {day}. Fighters: {players}',
		placeholders: ['boss', 'key', 'day', 'duration', 'wipes', 'players', 'server', 'event'],
		sample: {
			boss: 'Eikthyr',
			key: 'defeated_eikthyr',
			day: '12',
			duration: '95',
			wipes: '1',
			players: FIGHTERS
		}
	},
	{
		kind: 'raid_started',
		group: 'Raids',
		label: 'Raid started',
		when: 'A raid begins near players.',
		headline: 'Raid started',
		accent: '#D83C3E',
		template: '{name}. Players in the raid: {players}',
		placeholders: ['name', 'raid', 'players', 'server', 'event'],
		sample: { name: 'The ground is shaking', raid: 'army_eikthyr', players: 'Alex' }
	},
	{
		kind: 'raid_ended',
		group: 'Raids',
		label: 'Raid ended',
		when: 'The raid is over.',
		headline: 'Raid ended',
		accent: '#2ECC71',
		template: 'The raid is over: {name}',
		placeholders: ['name', 'raid', 'players', 'server', 'event'],
		sample: { name: 'The ground is shaking', raid: 'army_eikthyr', players: 'Alex' }
	}
];

export const GAME_KINDS: GameKind[] = GAME_EVENTS.map((e) => e.kind);

/** Whether a rule kind is a game event, which the ValminEye plugin reports. */
export function isGameKind(kind: string): kind is GameKind {
	return (GAME_KINDS as string[]).includes(kind);
}
