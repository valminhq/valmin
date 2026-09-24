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

Open **Mods** to search the catalogue, review the dependency plan, and apply changes.
Use **Settings files** to edit BepInEx and plugin configuration. Stop the server
before editing files. If a new mod has no
configuration file yet, start the server once so it can generate one.

### Choosing a registry

The catalogue is fed by two registries, **Thunderstore** and **Hexium**, and the
buttons above the search box pick which one you are browsing. **All** shows both.

Each listing is labelled with its registry and its version is shown in that
registry's colour, so the two are distinguishable at a glance and in a screenshot.
A mod both registries publish appears **twice**, once per registry, usually at
different versions — that is not a duplicate, it is the choice of which copy to
install. The registries are run by different people, so the same name and version
number on each is not a guarantee of the same files.

A server installs any given mod from one registry at a time. Where a mod is already
installed from the other one, its row says so and offers no install; uninstall it
first if you want to switch. An update is only ever offered from the registry the
installed files came from.

If a registry is switched off in the configuration, it disappears from search and
nothing new can be installed from it, but mods already installed from it keep
working and keep their details on this screen.

### Update every mod at once

When any installed mod has a newer version, the **Installed** tab shows
**Update all mods**. It lists every package that will change in one view: each
mod's current and new version, plus any new dependencies the updates need. After
you confirm, one job backs up the world, then updates the whole set. If any
package fails, the job rolls back every package and keeps the backup. The backup
appears on the **Backups** page as a pre-update backup. Stop the server first, as
for any mod change.

### When a mod does not load

After the server starts, the **Mods** page shows what the mod loader reported.
If the loader says it could not load a mod, that mod's row turns red and shows the
loader's message, such as a missing dependency or an error from the mod's own
code. The alert at the top of the page names every mod that failed. **Not loading**
means the loader did not mention the mod at all. Check the loader log for that
mod's name.

Leaving a page with unsaved edits asks you to confirm first, so a mistyped link
does not discard them. Saving clears the prompt.

Check the job result and any pending restart notice after changes. For mods that
also run on clients, export the client manifest to share the required versions
with players. Crossplay does not make a mod compatible with console clients.

The exported manifest names packages the way client mod managers expect, which has
no way to say which registry a package came from. A manager resolves those names
against Thunderstore, so a mod installed from Hexium and published nowhere else may
not be found on import. Players can still install it by hand from the registry's own
page.

## Share access

Administrators create invitations under **Invites** and manage accounts under
**Users**, both in the header's **Administration** menu. A member needs a grant on
a server before they can access it. Use the server's **Panel access** page to assign
a role and extra permissions.

Public server listing and the public status page are separate settings. Enable
**Public status page** in server settings to share an unauthenticated status link.
