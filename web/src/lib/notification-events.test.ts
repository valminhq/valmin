import { describe, expect, it } from 'vitest';
import { expand } from './message';
import { PANEL_EVENTS, withoutFields } from './notification-events';

describe('PANEL_EVENTS', () => {
	it('fills every placeholder of each default message from its sample', () => {
		for (const e of PANEL_EVENTS) {
			const vars = { ...e.sample, server: 'Midgard', event: e.headline };
			expect(expand(e.template, vars), e.kind).not.toMatch(/\{|\}/);
			for (const name of e.template.matchAll(/\{([a-z_]+)\}/g)) {
				expect(e.placeholders, e.kind).toContain(name[1]);
			}
		}
	});
});

describe('withoutFields', () => {
	it('drops the lines that show a hidden field', () => {
		expect(withoutFields('A: {a}\nB: {b}\nC', ['b'])).toBe('A: {a}\nC');
		expect(withoutFields('A: {a}', [])).toBe('A: {a}');
	});
});
