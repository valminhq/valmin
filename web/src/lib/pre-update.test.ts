import { describe, expect, it } from 'vitest';
import { preUpdateNotice } from './pre-update.js';

describe('preUpdateNotice', () => {
	it('names what the change saves first', () => {
		expect(preUpdateNotice({ backup: true, setup: true }, 'update')).toBe(
			'The world and the current setup are saved first. Both are kept even if the update fails.'
		);
		expect(preUpdateNotice({ backup: true, setup: false }, 'install')).toBe(
			'The world is backed up first. The backup is kept even if the install fails.'
		);
		expect(preUpdateNotice({ backup: false, setup: true }, 'install')).toBe(
			'The current setup is saved first, so the install can be undone from Saved setups.'
		);
		expect(preUpdateNotice({ backup: false, setup: false }, 'update')).toBeNull();
	});
});
