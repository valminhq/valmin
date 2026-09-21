import { describe, expect, it } from 'vitest';
import { catalogueStatus, modOffer, installedUpdateTarget } from './mod-catalogue';
import type { InstalledMod, ModSummary, RegistryStatus } from '$lib/api/mods';

const listing = { source: 'hexium', latest_version: '1.0.0' } as ModSummary;
const installed = { source: 'thunderstore', version: '2.0.0', update_version: '' } as InstalledMod;

describe('mod offers', () => {
	it('identifies another registry instead of offering an update or claiming its bytes are installed', () => {
		expect(modOffer(listing, installed)).toBe('other-source');
		expect(modOffer({ ...listing, latest_version: '2.0.0' }, installed)).toBe('other-source');
	});
	it('offers only the update version confirmed by the backend', () => {
		const sameSource = { ...installed, source: 'hexium' as const, update_version: '3.0.0' };
		expect(modOffer(listing, sameSource)).toBe('installed');
		expect(modOffer({ ...listing, latest_version: '3.0.0' }, sameSource)).toBe('update');
	});
	it('offers an install when the package is absent', () => {
		expect(modOffer(listing, undefined)).toBe('install');
	});
});

const statuses: RegistryStatus[] = [
	{ source: 'thunderstore', enabled: true, synced_at: '2026-09-21T12:00:00Z' },
	{ source: 'hexium', enabled: true, synced_at: null }
];

describe('catalogue status', () => {
	it('distinguishes a partial catalogue from the selected healthy registry', () => {
		expect(catalogueStatus(statuses, null)).toBe(
			'Partial catalogue. Waiting for Hexium to download.'
		);
		expect(catalogueStatus(statuses, 'thunderstore')).toBeNull();
		expect(catalogueStatus(statuses, 'hexium')).toBe('Waiting for Hexium to download.');
	});
	it('explains a disabled registry without promising a download', () => {
		const disabled = statuses.map((row) =>
			row.source === 'hexium' ? { ...row, enabled: false } : row
		);
		expect(catalogueStatus(disabled, 'hexium')).toBe('Hexium is disabled.');
		expect(catalogueStatus(disabled, null)).toBeNull();
	});
});

describe('installed update targets', () => {
	it('carries the approved version, source, and deprecation warning without a detail fetch', () => {
		const mod = {
			...installed,
			full_name: 'Author-Mod',
			name: 'Mod',
			update_version: '3.0.0',
			is_deprecated: true
		};
		expect(installedUpdateTarget(mod)).toEqual({
			full_name: 'Author-Mod',
			name: 'Mod',
			source: 'thunderstore',
			latest_version: '3.0.0',
			is_deprecated: true
		});
	});
	it('offers no target without an update and falls back to the full name for display', () => {
		expect(installedUpdateTarget(installed)).toBeNull();
		expect(
			installedUpdateTarget({
				...installed,
				full_name: 'Author-Mod',
				name: '',
				update_version: '3.0.0'
			})?.name
		).toBe('Author-Mod');
	});
});
