import { describe, expect, it } from 'vitest';
import type { InboxItem } from '$lib/api/inbox';
import { filterServers, type ServerStateFilter } from '$lib/server-filter';
import { instance } from '$lib/testing/daemon';

const servers = [
	instance({ id: 'a', name: 'Alpha Base', state: 'running' }),
	instance({ id: 'b', name: 'Bravo', state: 'stopped' }),
	instance({ id: 'c', name: 'Charlie', state: 'error' }),
	instance({ id: 'd', name: 'Delta', state: 'running' }),
	instance({ id: 'e', name: 'Echo', state: 'starting' })
];

const since_at = '2026-09-24T10:00:00Z';
const conditions: InboxItem[] = [
	{ kind: 'low_disk', severity: 'warning', instance_id: 'd', since_at },
	{ kind: 'restart_required', severity: 'warning', instance_id: 'b', since_at },
	{ kind: 'stale_backup', severity: 'critical', instance_id: null, since_at }
];

describe('filterServers', () => {
	const cases: Array<[name: string, query: string, state: ServerStateFilter, want: string[]]> = [
		['a blank query and all keep every server', '', 'all', ['a', 'b', 'c', 'd', 'e']],
		['whitespace alone is blank', '   ', 'all', ['a', 'b', 'c', 'd', 'e']],
		['matches part of a name', 'harl', 'all', ['c']],
		['ignores case', 'ALPHA', 'all', ['a']],
		['trims the query', '  bravo ', 'all', ['b']],
		['keeps running servers', '', 'running', ['a', 'd']],
		['keeps stopped servers', '', 'stopped', ['b']],
		['a transient state is neither running nor stopped', 'echo', 'running', []],
		['needs attention covers the error state and chips', '', 'attention', ['c', 'd']],
		['a condition the card already states is not a chip', 'bravo', 'attention', []],
		['a host condition marks no server', 'alpha', 'attention', []],
		['query and state combine', 'a', 'running', ['a', 'd']],
		['returns nothing when nothing matches', 'zzz', 'all', []]
	];

	it.each(cases)('%s', (_name, query, state, want) => {
		expect(filterServers(servers, conditions, query, state).map((s) => s.id)).toEqual(want);
	});

	it('keeps the order it was given', () => {
		const reversed = [...servers].reverse();
		expect(filterServers(reversed, conditions, '', 'all').map((s) => s.id)).toEqual([
			'e',
			'd',
			'c',
			'b',
			'a'
		]);
	});
});
