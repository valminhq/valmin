import { describe, expect, it } from 'vitest';
import { GAME_EVENTS, isGameKind } from './game-events';
import { expand } from './message';

describe('GAME_EVENTS', () => {
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
