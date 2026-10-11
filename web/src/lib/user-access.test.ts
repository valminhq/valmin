import { describe, expect, it } from 'vitest';
import type { Grant, GrantRole } from '$lib/api/grants';
import { serversReached, type ServerGrants } from './user-access';

const now = Date.parse('2026-09-29T12:00:00Z');

function grant(userId: string, role: GrantRole, expiresAt: string | null = null): Grant {
	return {
		user_id: userId,
		instance_id: 'inst',
		role,
		perms: [],
		granted_by: null,
		granted_at: '2026-09-01T00:00:00Z',
		expires_at: expiresAt,
		etag: '"v1"'
	};
}

const server = (id: string, name: string, grants: Grant[]): ServerGrants => ({ id, name, grants });

describe('servers a member reaches', () => {
	const cases: Array<[name: string, servers: ServerGrants[], expected: string[]]> = [
		['no servers', [], []],
		['servers with no grants', [server('a', 'Alpha', []), server('b', 'Beta', [])], []],
		[
			'grants held by other people only',
			[server('a', 'Alpha', [grant('u-other', 'operator')])],
			[]
		],
		[
			'one grant among several people',
			[server('a', 'Alpha', [grant('u-other', 'viewer'), grant('u-1', 'operator')])],
			['Alpha:operator']
		],
		[
			'every server, in the order given, each with its own role',
			[
				server('b', 'Beta', [grant('u-1', 'viewer')]),
				server('a', 'Alpha', [grant('u-1', 'operator')])
			],
			['Beta:viewer', 'Alpha:operator']
		],
		[
			'a server without a grant is skipped',
			[
				server('a', 'Alpha', [grant('u-1', 'viewer')]),
				server('b', 'Beta', []),
				server('c', 'Gamma', [grant('u-1', 'operator')])
			],
			['Alpha:viewer', 'Gamma:operator']
		],
		[
			'an expired grant',
			[server('a', 'Alpha', [grant('u-1', 'operator', '2026-09-29T11:59:59Z')])],
			[]
		],
		[
			'a grant that expires later',
			[server('a', 'Alpha', [grant('u-1', 'operator', '2026-09-30T00:00:00Z')])],
			['Alpha:operator']
		]
	];

	it.each(cases)('%s', (_name, servers, expected) => {
		const reached = serversReached('u-1', servers, now).map((s) => `${s.name}:${s.role}`);
		expect(reached).toEqual(expected);
	});

	it('carries the server id so a name can link to its access page', () => {
		const [reached] = serversReached('u-1', [server('a', 'Alpha', [grant('u-1', 'viewer')])], now);
		expect(reached).toEqual({ id: 'a', name: 'Alpha', role: 'viewer' });
	});
});
