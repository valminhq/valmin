import { describe, expect, it } from 'vitest';
import type { InboxItem, InboxKind } from '$lib/api/inbox';
import { ALREADY_ON_CARD, bandItems, chipsFor, conditionAge, formatBytes } from '$lib/conditions';

function item(kind: InboxKind, over: Partial<InboxItem> = {}): InboxItem {
	return {
		kind,
		severity: 'warning',
		instance_id: 'one',
		since_at: '2026-09-20T12:00:00Z',
		...over
	};
}

describe('chipsFor', () => {
	it('takes only the named server conditions', () => {
		const items = [
			item('update_available', { instance_id: 'one' }),
			item('job_failed', { instance_id: 'two' }),
			item('low_disk', { instance_id: null })
		];
		expect(chipsFor(items, 'one').map((c) => c.kind)).toEqual(['update_available']);
	});

	it('drops the kinds the card already states through its state badge', () => {
		const items = ALREADY_ON_CARD.map((kind) => item(kind)).concat(item('update_available'));
		expect(chipsFor(items, 'one').map((c) => c.kind)).toEqual(['update_available']);
	});

	it('orders critical before warning, then longest-standing first', () => {
		const items = [
			item('update_available', { since_at: '2026-09-20T11:00:00Z' }),
			item('crash_loop', { severity: 'critical', since_at: '2026-09-20T11:30:00Z' }),
			item('job_failed', { since_at: '2026-09-20T09:00:00Z' }),
			item('unclean_stop', { severity: 'critical', since_at: '2026-09-20T10:00:00Z' })
		];
		expect(chipsFor(items, 'one').map((c) => c.kind)).toEqual([
			'unclean_stop',
			'crash_loop',
			'job_failed',
			'update_available'
		]);
	});

	it('does not reorder or consume the array it was given', () => {
		const items = [
			item('update_available'),
			item('crash_loop', { severity: 'critical', since_at: '2026-09-20T13:00:00Z' })
		];
		const before = items.map((c) => c.kind);
		chipsFor(items, 'one');
		expect(items.map((c) => c.kind)).toEqual(before);
	});
});

describe('bandItems', () => {
	it('carries the conditions that belong to no server', () => {
		const items = [item('low_disk', { instance_id: null }), item('update_available')];
		expect(bandItems(items, ['one']).map((c) => c.kind)).toEqual(['low_disk']);
	});

	// Without this a condition on a server the list has not rendered would be shown nowhere,
	// which for a critical one is worse than showing it twice.
	it('catches a condition whose server is not on the list', () => {
		const items = [item('crash_loop', { severity: 'critical', instance_id: 'missing' })];
		expect(bandItems(items, ['one']).map((c) => c.kind)).toEqual(['crash_loop']);
	});

	it('leaves nothing in the band once every server is listed', () => {
		const items = [item('update_available', { instance_id: 'two' })];
		expect(bandItems(items, ['one', 'two'])).toEqual([]);
	});
});

describe('formatBytes', () => {
	it('scales to a readable unit', () => {
		expect(formatBytes(String(900))).toBe('900 B');
		expect(formatBytes(String(6.4 * 1024 * 1024))).toBe('6.4 MB');
		expect(formatBytes(String(2 * 1024 ** 3))).toBe('2.0 GB');
	});

	it('says nothing rather than NaN when the daemon sent no number', () => {
		expect(formatBytes(undefined)).toBe('');
		expect(formatBytes('unknown')).toBe('');
	});
});

describe('conditionAge', () => {
	it('reads in the largest unit that still says something', () => {
		const now = Date.parse('2026-09-20T12:00:00Z');
		expect(conditionAge(item('job_failed', { since_at: '2026-09-20T11:45:00Z' }), now)).toBe('15m');
		expect(conditionAge(item('job_failed', { since_at: '2026-09-20T06:00:00Z' }), now)).toBe('6h');
		expect(conditionAge(item('job_failed', { since_at: '2026-09-17T12:00:00Z' }), now)).toBe('3d');
	});
});
