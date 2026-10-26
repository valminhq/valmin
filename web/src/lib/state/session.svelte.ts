import { api, onUnauthenticated } from '#lib/api/client.js';
import { ApiError } from '#lib/api/errors.js';
import type { MyPermissions, User } from '#lib/api/types.js';

/**
 * Who is signed in and what they may do.
 *
 * `allowed` is what the UI renders from — never `user.role` (F3, `09 §4.2`). Client-side
 * hiding is cosmetic; the server checks every request regardless, so a role branch here
 * would be a second, weaker copy of an authorization decision that already exists.
 */
class Session {
	user = $state<User | null>(null);
	permissions = $state<MyPermissions | null>(null);
	/** True until the first load settles, so the shell does not flash a login form. */
	loading = $state(true);
	/** Set when the panel has no admin yet (`10 §6`): every route answers 503 until setup. */
	setupRequired = $state(false);
	/** True once the permission set is known or the load that would supply it has finished. A
	 * boolean derived, so a screen reading it re-runs once rather than on every re-read. */
	ready = $derived(this.permissions !== null || !this.loading);

	async load(): Promise<void> {
		this.loading = true;
		try {
			// Read together, and applied together: signed out, the permissions read is refused and
			// leaves the set empty, and a late reply never outlives a failed sign-in check.
			const [user, permissions] = await Promise.all([
				api.get<User>('/auth/me'),
				api.get<MyPermissions>('/me/permissions').catch(() => null)
			]);
			this.user = user;
			this.permissions = permissions;
			this.setupRequired = false;
		} catch (err) {
			this.user = null;
			this.permissions = null;
			this.setupRequired = err instanceof ApiError && err.code === 'setup_required';
		} finally {
			this.loading = false;
		}
	}

	/** Re-reads the permission set. */
	async refreshPermissions(): Promise<void> {
		try {
			this.permissions = await api.get<MyPermissions>('/me/permissions');
		} catch {
			this.permissions = null;
		}
	}

	/** The global capabilities the signed-in user holds — what a create button renders from. */
	allowedGlobally(): string[] {
		return this.permissions?.allowed_actions ?? [];
	}

	/** The actions the signed-in user holds on one instance, or none. */
	allowed(instanceId: string): string[] {
		return (
			this.permissions?.instances.find((i) => i.instance_id === instanceId)?.allowed_actions ?? []
		);
	}

	can(instanceId: string, action: string): boolean {
		return this.allowed(instanceId).includes(action);
	}

	signedIn(user: User): void {
		this.user = user;
		this.setupRequired = false;
		void this.refreshPermissions();
	}

	signedOut(): void {
		this.user = null;
		this.permissions = null;
	}
}

export const session = new Session();

// A session that ended elsewhere (another tab signed out, idle expiry, an account change)
// signs this tab out on its next request, and the root layout then sends it to sign in.
onUnauthenticated(() => {
	if (session.user) session.signedOut();
});
