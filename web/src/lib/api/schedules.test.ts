import { describe, expect, it } from 'vitest';
import { kindLabel } from './schedules';

describe('kindLabel', () => {
	it.each([
		{ kind: 'backup', want: 'Back up this server' },
		{ kind: 'game_update', want: 'Update the game' },
		{ kind: 'update_check', want: 'update check' }
	])('labels $kind', ({ kind, want }) => {
		expect(kindLabel(kind)).toBe(want);
	});
});
