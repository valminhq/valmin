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
		render(RestartNotice, { instance: { state: 'running', ...flags }, canRestart: true });
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

	// A stopped server applies its changes on its next start, and a viewer cannot restart.
	it.each([
		['a stopped server', 'stopped', true],
		['a viewer', 'running', false]
	])('says nothing for %s', (_name, state, canRestart) => {
		const flags = { restart_required: true, pending_restart: true };
		render(RestartNotice, { instance: { state, ...flags }, canRestart });
		render(StateBadge, { state, restartRequired: canRestart, pendingRestart: state === 'stopped' });
		expect(screen.queryByText('Restart to apply settings')).toBeNull();
		// The badge keeps a launch change visible on a stopped server, as its alert is.
		expect(screen.queryByText('pending restart')).toBeNull();
		expect(!!screen.queryByText('restart required')).toBe(canRestart);
	});
});
