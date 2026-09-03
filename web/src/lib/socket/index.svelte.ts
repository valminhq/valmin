import { goto } from '$app/navigation';
import { resolve } from '$app/paths';
import { session } from '$lib/state/session.svelte';
import { Socket, type SocketStatus } from './client';

/**
 * The panel's one socket, and the one place `14 §6`'s close codes are acted on.
 *
 * `4401` and `4403` are handled differently on purpose. A session that expired means
 * sign in again; a grant that was narrowed means read the permissions again and carry on.
 * Treating them alike signs a user out because an admin edited someone's access.
 */
export const socketStatus = $state<{ value: SocketStatus }>({ value: 'closed' });

export const socket = new Socket({
	onSessionExpired: () => {
		session.signedOut();
		void goto(resolve('/login'));
	},
	onAccessRevoked: () => void session.refreshPermissions(),
	onStatus: (status) => {
		socketStatus.value = status;
	}
});
