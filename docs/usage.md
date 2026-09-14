# Use the panel

[Documentation](README.md) / Use the panel

## Create a server

1. Select **New server** from the server list.
2. Enter the server name, world name, and server password. The password must have
   at least five characters and must not appear inside either name.
3. Choose public listing, crossplay, and gameplay settings. Add mods if needed.
4. Submit the form and follow the provisioning job. The first installation downloads
   the dedicated server through SteamCMD; later installations reuse the build cache.
5. Start the server if you did not select the option to start it after creation.
   Wait for the panel to report it ready before connecting.

Connect using the host's address and the assigned base port, or the join code
shown for a crossplay server. The server password is separate from your panel
account password.

## Import a world

The creation form accepts an existing save. Choose the world folder under
`worlds_local` for a folder-based save, or both `.db` and `.fwl` files for a legacy
save. Keep the complete folder for saves that include chunk files.

When importing during creation, keep the page open until provisioning and import
finish. The server stays stopped during import. You can also import into an
existing stopped server from **Server settings**.

## Delete a world or start one over

**Backups** lists the worlds in the server's save directory. Stop the server, then
use **Delete** beside a world to remove it. Deleting the world the server loads
resets it: the server generates a new world the next time it starts.

The whole save directory is backed up before anything is removed, so a deletion can
be undone from the backups list.

## Manage mods and configuration

Open **Mods** to search Thunderstore, review the dependency plan, and apply changes.
Use **Settings files** to edit BepInEx and plugin configuration. Stop the server
before editing files. If a new mod has no
configuration file yet, start the server once so it can generate one.

Check the job result and any pending restart notice after changes. For mods that
also run on clients, export the client manifest to share the required versions
with players. Crossplay does not make a mod compatible with console clients.

## Share access

Administrators create invitations under **Invites** and manage accounts under
**Users**. A member needs a grant on a server before they can access it. Use the
server's **Panel access** page to assign a role and extra permissions.

Public server listing and the public status page are separate settings. Enable
**Public status page** in server settings to share an unauthenticated status link.
