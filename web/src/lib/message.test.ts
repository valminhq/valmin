import { describe, expect, it } from 'vitest';
import { DEFAULT_STYLE, expand, styleOf, styleParams } from './message';

describe('expand', () => {
	it('fills placeholders, empties unknown ones and keeps other braces', () => {
		const vars = { player: 'Alex', day: '12' };
		expect(expand('{player} joined on day {day}', vars)).toBe('Alex joined on day 12');
		expect(expand('{player} died {deaths} times', vars)).toBe('Alex died  times');
		expect(expand('{Player} {} {player:x} {{player}}', vars)).toBe('{Player} {} {player:x} {Alex}');
	});

	it('leaves out a line whose every placeholder is empty', () => {
		const vars = { player: 'Alex', day: '12' };
		expect(expand('**Player:** {player}\n**Deaths:** {deaths}\nGood luck', vars)).toBe(
			'**Player:** Alex\nGood luck'
		);
		expect(expand('{deaths}', vars)).toBe('');
		expect(expand('Fighters:\n{deaths} {wipes}\n\n{day}', vars)).toBe('Fighters:\n\n12');
	});
});

describe('message style', () => {
	it('stores only what differs from the default', () => {
		expect(styleParams(DEFAULT_STYLE)).toEqual({});
		const style = { layout: 'plain', hideLink: true, hideTime: false, color: '#123456' } as const;
		expect(styleParams(style)).toEqual({ layout: 'plain', hide_link: true, color: '#123456' });
		expect(styleOf(styleParams(style))).toEqual(style);
		expect(styleOf({})).toEqual(DEFAULT_STYLE);
	});
});
