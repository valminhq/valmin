import { describe, expect, it } from 'vitest';
import { expiresIn, expiringSoon, inviteStatus, type InviteStatus } from './invite-status';

const now = Date.parse('2026-09-20T12:00:00Z');

describe('inviteStatus', () => {
	const cases: Array<{
		name: string;
		expires_at: string;
		redeemed_at?: string;
		revoked_at?: string;
		want: InviteStatus;
	}> = [
		{ name: 'unused and in date', expires_at: '2026-09-21T12:00:00Z', want: 'active' },
		{ name: 'a millisecond before expiry', expires_at: '2026-09-20T12:00:00.001Z', want: 'active' },
		{ name: 'at the instant of expiry', expires_at: '2026-09-20T12:00:00Z', want: 'expired' },
		{ name: 'past expiry', expires_at: '2026-09-19T12:00:00Z', want: 'expired' },
		{
			name: 'redeemed before it expired',
			expires_at: '2026-09-19T12:00:00Z',
			redeemed_at: '2026-09-18T12:00:00Z',
			want: 'redeemed'
		},
		{
			name: 'revoked while in date',
			expires_at: '2026-09-21T12:00:00Z',
			revoked_at: '2026-09-20T11:00:00Z',
			want: 'revoked'
		},
		{
			name: 'revoked and since expired',
			expires_at: '2026-09-19T12:00:00Z',
			revoked_at: '2026-09-18T12:00:00Z',
			want: 'revoked'
		},
		{
			name: 'redeemed and later revoked',
			expires_at: '2026-09-21T12:00:00Z',
			redeemed_at: '2026-09-20T10:00:00Z',
			revoked_at: '2026-09-20T11:00:00Z',
			want: 'redeemed'
		}
	];

	for (const c of cases) {
		it(c.name, () => {
			const invite = {
				expires_at: c.expires_at,
				redeemed_at: c.redeemed_at ?? null,
				revoked_at: c.revoked_at ?? null
			};
			expect(inviteStatus(invite, now)).toBe(c.want);
		});
	}
});

describe('expiresIn', () => {
	const cases: Array<[name: string, expiresAt: string, want: string]> = [
		['seconds away', '2026-09-20T12:00:30Z', 'in under a minute'],
		['one minute', '2026-09-20T12:01:00Z', 'in 1 minute'],
		['under an hour', '2026-09-20T12:45:00Z', 'in 45 minutes'],
		['one hour', '2026-09-20T13:00:00Z', 'in 1 hour'],
		['under a day', '2026-09-21T11:00:00Z', 'in 23 hours'],
		['one day', '2026-09-21T12:00:00Z', 'in 1 day'],
		['several days', '2026-09-27T12:00:00Z', 'in 7 days'],
		['a moment short of a week', '2026-09-27T11:59:59Z', 'in 7 days'],
		['a day and a half', '2026-09-22T00:00:00Z', 'in 2 days']
	];

	for (const [name, expiresAt, want] of cases) {
		it(name, () => expect(expiresIn(expiresAt, now)).toBe(want));
	}
});

describe('expiringSoon', () => {
	const cases: Array<[name: string, expiresAt: string, want: boolean]> = [
		['within the hour', '2026-09-20T12:30:00Z', true],
		['a millisecond inside a day', '2026-09-21T11:59:59.999Z', true],
		['exactly a day away', '2026-09-21T12:00:00Z', false],
		['several days away', '2026-09-25T12:00:00Z', false]
	];

	for (const [name, expiresAt, want] of cases) {
		it(name, () => expect(expiringSoon(expiresAt, now)).toBe(want));
	}
});
