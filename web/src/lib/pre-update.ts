/** What a mod change saves before it touches any file, as the daemon's preview reports it. */
export interface PreUpdateSaves {
	/** The world is archived first. */
	backup: boolean;
	/** The current setup is saved first as an automatic one. */
	setup: boolean;
}

/**
 * The sentence telling the operator what a mod change saves first, naming the change as
 * `change` ("update", "install"…), or null when it saves nothing.
 */
export function preUpdateNotice(saves: PreUpdateSaves, change: string): string | null {
	if (saves.backup && saves.setup) {
		return `The world and the current setup are saved first. Both are kept even if the ${change} fails.`;
	}
	if (saves.backup) {
		return `The world is backed up first. The backup is kept even if the ${change} fails.`;
	}
	if (saves.setup) {
		return 'This server has no world yet, so only the current setup is saved first.';
	}
	return null;
}
