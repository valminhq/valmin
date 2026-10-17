import { describe, expect, it } from 'vitest';
import { GAME_EVENTS, expand, isGameKind } from './game-events';

describe('expand', () => {
	it('fills placeholders, empties unknown ones and keeps other braces', () => {
		const vars = { player: 'Alex', day: '12' };
		expect(expand('{player} joined on day {day}', vars)).toBe('Alex joined on day 12');
		expect(expand('{player} died {deaths} times', vars)).toBe('Alex died  times');
		expect(expand('{Player} {} {player:x} {{player}}', vars)).toBe('{Player} {} {player:x} {Alex}');
	});

	it('leaves no placeholder unfilled in any default message', () => {
		for (const e of GAME_EVENTS) {
			expect(expand(e.template, { ...e.sample, server: 'Midgard' })).not.toMatch(/\{|\}/);
			for (const name of e.template.matchAll(/\{([a-z_]+)\}/g)) {
				expect(e.placeholders).toContain(name[1]);
			}
		}
	});
});

describe('isGameKind', () => {
	it('tells game events from other rule kinds', () => {
		expect(isGameKind('player_joined')).toBe(true);
		expect(isGameKind('server_started')).toBe(false);
	});
});
