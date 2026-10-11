import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import RestartNotice from './restart-notice.svelte';
import StateBadge from './state-badge.svelte';

describe('the restart notice and badge', () => {
	it.each([
		[
			{ restart_required: true, pending_restart: true },
			'Restart to apply settings',
			'restart required'
		],
		[
			{ restart_required: false, pending_restart: true },
			'Changes apply on next start',
			'pending restart'
		],
		[{ restart_required: false, pending_restart: false }, null, null]
	])('for %o says %s', (flags, notice, badge) => {
		render(RestartNotice, { instance: flags });
		render(StateBadge, {
			state: 'running',
			restartRequired: flags.restart_required,
			pendingRestart: flags.pending_restart
		});
		for (const words of [
			'Restart to apply settings',
			'Changes apply on next start',
			'restart required',
			'pending restart'
		]) {
			const shown = words === notice || words === badge;
			expect(!!screen.queryByText(words), words).toBe(shown);
		}
	});
});
