import { render, screen } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReloadNotice from './reload-notice.svelte';

const build = vi.hoisted(() => ({ updated: false }));

vi.mock('$app/state', () => ({
	updated: {
		get current() {
			return build.updated;
		}
	}
}));

describe('the reload notice', () => {
	beforeEach(() => {
		build.updated = false;
	});

	it('is absent while this tab runs the current build', () => {
		render(ReloadNotice);
		expect(screen.queryByText('Valmin was updated')).toBeNull();
	});

	it('offers a reload once a newer build is deployed', () => {
		build.updated = true;
		render(ReloadNotice);
		expect(screen.getByText('Valmin was updated')).toBeTruthy();
		expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy();
	});
});
