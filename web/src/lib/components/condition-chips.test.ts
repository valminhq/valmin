import { describe, expect, it, vi } from 'vitest';
import { render } from 'svelte/server';
import type { InboxItem, InboxKind } from '$lib/api/inbox';
import ConditionChips from './condition-chips.svelte';

vi.mock('$app/paths', () => ({
	resolve: (route: string, params: { id: string }) => route.replace('[id]', params.id)
}));

function item(kind: InboxKind, over: Partial<InboxItem> = {}): InboxItem {
	return {
		kind,
		severity: 'warning',
		instance_id: 'one',
		since_at: new Date(Date.now() - 15 * 60_000).toISOString(),
		...over
	};
}

const body = (items: InboxItem[]) => render(ConditionChips, { props: { items } }).body;

describe('condition chips', () => {
	it('renders nothing when a server has no conditions', () => {
		expect(body([])).not.toContain('<details');
	});

	it('puts the chips in one disclosure rather than one tab stop each', () => {
		const html = body([item('crash_loop', { severity: 'critical' }), item('stale_backup')]);
		expect((html.match(/<details/g) ?? []).length).toBe(1);
		expect(html).toContain('<summary');
	});

	it('renders the explanation as content, not only as a title attribute', () => {
		const html = body([item('low_disk', { detail: { Free: '1073741824', Alarm: '5368709120' } })]);
		expect(html).toContain('The host is running out of disk space');
		expect(html).toContain('1.0 GB free, below the 5.0 GB floor');
		expect(html, 'a hover-only tooltip is what this replaced').not.toContain('title=');
	});

	it('separates the sentence from its detail', () => {
		const html = body([item('low_disk', { detail: { Free: '1073741824', Alarm: '5368709120' } })]);
		// SSR writes block boundaries as comments, so the assertion is on what a reader sees.
		// The whitespace around a block tag is trimmed, and a separator written as markup then
		// runs the sentence into the detail: "…disk space· 1.0 GB free".
		const text = html.replaceAll(/<!--.*?-->/g, '');
		expect(text).toContain('disk space · 1.0 GB free');
		expect(text, 'nothing runs into a separator').not.toMatch(/\S·/);
	});

	it('says how long each condition has been open', () => {
		expect(body([item('unclean_stop')])).toContain('15m');
	});

	it('links a condition to where it is acted on', () => {
		expect(body([item('stale_backup')])).toContain('href="/instances/one/backups"');
		expect(body([item('update_available')])).toContain('href="/instances/one"');
	});

	it('offers no link for a condition with nowhere of its own to go', () => {
		expect(body([item('low_disk', { instance_id: null })])).not.toContain('<a ');
	});

	it('marks a critical condition with more than colour', () => {
		expect(body([item('crash_loop', { severity: 'critical' })])).toContain('aria-hidden="true"');
	});
});
