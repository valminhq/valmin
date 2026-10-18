import { session } from '$lib/state/session.svelte';
import { Socket, type SocketStatus } from './client';

/**
 * The panel's one socket, and the one place its close codes are acted on. A `4401` signs the
 * session out, and the root layout then sends the visitor to sign in. A topic error re-reads
 * the permissions, so a revoked server's controls disappear.
 */
export const socketStatus = $state<{ value: SocketStatus }>({ value: 'closed' });

export const socket = new Socket({
	onSessionExpired: () => session.signedOut(),
	onTopicError: () => void session.refreshPermissions(),
	onStatus: (status) => {
		socketStatus.value = status;
	}
});
