import { describe, expect, it } from 'vitest';
import { loginWithReturn, returnPath } from './nav';

const at = (href: string) => new URL(href, 'https://panel.example');

describe('returnPath', () => {
	it('returns a same-origin path unchanged, query included', () => {
		expect(returnPath(at('/login?next=%2Finstances%2Fabc%2Fbackups'))).toBe(
			'/instances/abc/backups'
		);
		expect(returnPath(at('/login?next=%2Fadmin%2Faudit%3Fkind%3Dstart'))).toBe(
			'/admin/audit?kind=start'
		);
	});

	it('falls back to the server list when there is nothing to return to', () => {
		expect(returnPath(at('/login'))).toBe('/');
		expect(returnPath(at('/login?next='))).toBe('/');
	});

	it('refuses any destination that leaves this origin', () => {
		for (const next of [
			'https://evil.example/steal',
			'//evil.example/steal',
			'/\\evil.example',
			'http://panel.example/instances',
			'javascript:alert(1)',
			'instances/abc'
		]) {
			expect(returnPath(at(`/login?next=${encodeURIComponent(next)}`)), next).toBe('/');
		}
	});
});

describe('loginWithReturn', () => {
	it('carries the path being left, with its query', () => {
		expect(loginWithReturn('/login', at('/instances/abc/backups'))).toBe(
			'/login?next=%2Finstances%2Fabc%2Fbackups'
		);
		expect(loginWithReturn('/login', at('/admin/audit?kind=start'))).toBe(
			'/login?next=%2Fadmin%2Faudit%3Fkind%3Dstart'
		);
	});

	it('adds nothing when the destination is already the default', () => {
		expect(loginWithReturn('/login', at('/'))).toBe('/login');
	});
});
