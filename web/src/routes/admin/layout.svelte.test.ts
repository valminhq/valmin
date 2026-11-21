import { render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { actions } from '#lib/api/instances.js';
import { session } from '#lib/state/session.svelte.js';
import { permissions } from '#lib/testing/daemon.js';
import Layout from './+layout.svelte';

vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/admin/users') } }));

const children = createRawSnippet(() => ({ render: () => '<p>Create user</p>' }));

afterEach(() => {
	session.permissions = null;
});

describe('the admin layout', () => {
	// The daemon answers 404 to a member; the page must not render its forms around that.
	it('shows a member the not-found page instead of the admin page', () => {
		session.permissions = permissions('inst-a', [actions.statsRead], []);
		render(Layout, { children });
		expect(screen.getByText('Page not found')).toBeTruthy();
		expect(screen.queryByText('Create user')).toBeNull();
	});

	it('renders the page for a holder of its action', () => {
		session.permissions = permissions('', [], [actions.usersManage]);
		render(Layout, { children });
		expect(screen.getByText('Create user')).toBeTruthy();
	});
});
