import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import type { Instance } from '$lib/api/instances';
import ConnectionSummary from './connection-summary.svelte';

const instance = (over: Partial<Instance> = {}): Instance =>
	({
		id: 'server-a',
		name: 'Friday Vikings',
		state: 'running',
		base_port: 2456,
		server_name: 'Friday',
		world_name: 'Midgard',
		public: false,
		crossplay: false,
		crossplay_join_code: null,
		...over
	}) as Instance;

const body = (over: Partial<Instance> = {}) =>
	render(ConnectionSummary, { props: { instance: instance(over) } }).body;

describe('the connection summary', () => {
	it('offers the join code and a way to take it', () => {
		const html = body({ crossplay: true, crossplay_join_code: '123456' });
		expect(html).toContain('123456');
		expect(html).toContain('Copy');
	});

	it('says why a crossplay code is missing, rather than leaving a gap', () => {
		expect(body({ crossplay: true, state: 'stopped' })).toContain(
			'Available once the server is running'
		);
		expect(body({ crossplay: true, state: 'running' })).toContain('Not logged yet');
	});

	it('offers no join code on a server that has no crossplay', () => {
		const html = body({ crossplay: false, crossplay_join_code: '123456' });
		expect(html, 'the field can carry a stale code from a crossplay past').not.toContain('123456');
	});

	it('names the port and claims no address for it', () => {
		const html = body();
		expect(html).toContain('2456');
		expect(html).toContain('2457');
		// `02 §5`: the panel publishes a host port and knows nothing about how a player reaches
		// the host. An address rendered here would be one the panel guessed.
		expect(html).toMatch(/does\s+not know that address/);
		expect(html, 'no host, no literal address').not.toMatch(/\b\d{1,3}(\.\d{1,3}){3}\b/);
	});

	it('mentions the community browser only for a listed server', () => {
		expect(body({ public: true })).toContain('Community browser');
		expect(body({ public: false })).not.toContain('Community browser');
	});
});
