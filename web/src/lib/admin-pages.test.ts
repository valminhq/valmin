import { describe, expect, it } from 'vitest';
import { actions } from '#lib/api/instances.js';
import { mayOpenAdminPage } from './admin-pages';

describe('admin page access', () => {
	it('opens a page only for a holder of its action', () => {
		expect(mayOpenAdminPage('/admin/users', [actions.usersManage])).toBe(true);
		expect(mayOpenAdminPage('/admin/users', [actions.auditRead])).toBe(false);
		expect(mayOpenAdminPage('/admin/diagnostics', [actions.panelSettings])).toBe(true);
		expect(mayOpenAdminPage('/admin/no-such-page', [actions.panelSettings])).toBe(false);
	});
});
