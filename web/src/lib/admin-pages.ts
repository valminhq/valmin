import { actions } from '#lib/api/instances.js';

/**
 * The global action each admin page needs, keyed by its path segment under /admin. The header
 * shows a link and the admin layout renders a page only for a holder of its action.
 */
export const adminPageActions: Record<string, string> = {
	users: actions.usersManage,
	invites: actions.invitesManage,
	players: actions.playersManage,
	audit: actions.auditRead,
	shutdowns: actions.schedulesGlobal,
	'remote-backups': actions.panelSettings,
	'rclone-callback': actions.panelSettings,
	webhooks: actions.panelSettings,
	discord: actions.panelSettings,
	diagnostics: actions.panelSettings,
	export: actions.panelSettings,
	cache: actions.panelSettings,
	keys: actions.panelSettings
};

/** Whether a holder of granted may open the admin page at pathname. */
export function mayOpenAdminPage(pathname: string, granted: string[]): boolean {
	const action = adminPageActions[pathname.split('/')[2] ?? ''];
	return action !== undefined && granted.includes(action);
}
