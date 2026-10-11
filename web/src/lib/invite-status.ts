import type { Invite } from '$lib/api/admin';

/** Where an invite stands: still redeemable, or which way it stopped being so. */
export type InviteStatus = 'active' | 'redeemed' | 'revoked' | 'expired';

/**
 * The status of an invite at `now` (epoch milliseconds). A redemption outranks a later revoke,
 * and an invite stops being redeemable at the instant it expires.
 */
export function inviteStatus(
	invite: Pick<Invite, 'expires_at' | 'redeemed_at' | 'revoked_at'>,
	now: number
): InviteStatus {
	if (invite.redeemed_at) return 'redeemed';
	if (invite.revoked_at) return 'revoked';
	return Date.parse(invite.expires_at) <= now ? 'expired' : 'active';
}

const day = 86_400_000;
const units: Array<[unit: Intl.RelativeTimeFormatUnit, seconds: number]> = [
	['day', 86400],
	['hour', 3600],
	['minute', 60]
];
const relative = new Intl.RelativeTimeFormat('en', { numeric: 'always' });

/** How long after `now` (epoch milliseconds) an invite expires, to the nearest whole unit: "in 2 days". */
export function expiresIn(expiresAt: string, now: number): string {
	const seconds = (Date.parse(expiresAt) - now) / 1000;
	for (const [unit, size] of units) {
		if (seconds >= size) return relative.format(Math.round(seconds / size), unit);
	}
	return 'in under a minute';
}

/** Whether an invite expires within a day of `now` (epoch milliseconds). */
export function expiringSoon(expiresAt: string, now: number): boolean {
	return Date.parse(expiresAt) - now < day;
}
