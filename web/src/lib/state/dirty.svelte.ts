import { SvelteSet } from 'svelte/reactivity';

/**
 * The editors that currently hold unsaved work. Membership is owned by the effect `unsaved`
 * installs, so an editor never has to remember to deregister — unmounting or saving removes
 * it. The root layout reads this to guard navigation.
 */
const dirty = new SvelteSet<object>();

/**
 * Registers an editor as holding unsaved work for as long as `is` returns true. Called once
 * at component init with a getter over whatever dirty state that editor already computes;
 * a successful save clears the registration by making the getter false.
 */
export function unsaved(is: () => boolean): void {
	const key = {};
	$effect(() => {
		if (is()) dirty.add(key);
		else dirty.delete(key);
		return () => dirty.delete(key);
	});
}

export function hasUnsaved(): boolean {
	return dirty.size > 0;
}

/**
 * Forgets every registration. Only the navigation guard calls this, on the branch where the
 * operator chose to leave: the editors unmount after the navigation starts, so they cannot
 * clear themselves in time to stop the guard firing again.
 */
export function discardUnsaved(): void {
	dirty.clear();
}
