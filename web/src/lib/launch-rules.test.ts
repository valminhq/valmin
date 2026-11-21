import { describe, expect, it } from 'vitest';
import { passwordInName, validJoinHost } from './launch-rules';

describe('launch rules', () => {
	it('accepts a host name or address and refuses a port or scheme', () => {
		for (const host of ['play.example.com', 'localhost', '203.0.113.7', '2001:db8::1', '::1']) {
			expect(validJoinHost(host), host).toBe(true);
		}
		for (const host of [
			'http://play.example.com',
			'play.example.com:2456',
			'203.0.113.7:2456',
			'play example.com',
			'-bad.example.com',
			'a..b'
		]) {
			expect(validJoinHost(host), host).toBe(false);
		}
	});

	it('finds a password inside either name, case-sensitively', () => {
		expect(passwordInName('Viking', 'Vikings Home', 'Midgard')).toBe(true);
		expect(passwordInName('gard', 'Vikings Home', 'Midgard')).toBe(true);
		expect(passwordInName('viking', 'Vikings Home', 'Midgard')).toBe(false);
		expect(passwordInName('', 'Vikings Home', 'Midgard')).toBe(false);
	});
});
