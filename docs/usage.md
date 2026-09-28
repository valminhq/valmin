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

To look up the server password, open the server's overview and select **Show** in
the connection card, then copy it from there. Anyone who can view the server can
read it, and each read is recorded in the audit log. To change it, enter a new one
in **Settings**; the running server keeps the old password until it restarts.

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

If the server was started outside the panel, for example with `docker start`,
imports, deletions and restores fail without changing the world. Stop it and retry.

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

Updating a single mod, or installing one that raises an installed package to a new
version (a newer BepInEx, for example), shows each changed package's current and new
version. On a server with a world, the job backs up the world first and keeps the
backup even if the update fails.

If a registry is switched off in the configuration, it disappears from search and
nothing new can be installed from it, but mods already installed from it keep
working and keep their details on this screen.

### Choose a mod version

Choose **Version** on an installed mod to move it to another version, older ones
included. The version list comes from the registry the mod was installed from. Before
anything changes, Valmin shows every package that would be installed, updated,
downgraded, or removed. If the change would break another installed mod, for example a
downgrade below what that mod needs, Valmin names the mod and will not apply the change.

A downgrade changes mod files only. It does not restore the world or the config files,
and a world or setting that a newer version changed may not work with the older one. On a
server with a world, Valmin backs up the world first, so restore that backup if you need
to go back completely.

### Lock a mod version

Choose **Lock** on a mod to keep it at its current version. **Update all mods** skips a
locked mod, and an install never moves it: if another mod needs a different version of
it, Valmin explains the conflict and does not apply the change. Choose **Unlock** to
allow changes again. Locking works while the server is running, and every lock and unlock
is recorded in the audit log. Installing a mod raises BepInEx to its newest version, so lock
BepInEx if you need to keep an older one.

### Modpacks

A modpack is a package that bundles other mods, each at a fixed version. The installed
list marks it as a **modpack**, and each mod it includes says which modpack it belongs to.
To update a modpack, choose **Update** or **Version** on the modpack's own row. The
preview shows how each of its mods changes:

- A mod that still matches the modpack's version moves to the new modpack's version,
  even when that is older.
- A mod you locked, or installed or changed yourself, keeps its version. The preview
  lists it under the changes of yours that stay.
- A mod the new modpack version no longer includes is removed, unless you installed it
  yourself, locked it, or another mod still needs it.

Other mods change only when a mod in the modpack needs a newer version of one, as with
any install. **Update all mods** leaves modpacks and the mods they manage alone.

### Update every mod at once

When any installed mod has a newer version, the **Installed** tab shows
**Update all mods**. It lists every package that will change in one view: each
mod's current and new version, plus any new dependencies the updates need. Locked mods,
modpacks, and the mods a modpack manages are left out. After
you confirm, one job backs up the world, then updates the whole set. If any
package fails, the job rolls back every package and keeps the backup. The backup
appears on the **Backups** page as a pre-update backup. Stop the server first, as
for any mod change.

A mod marked **not in the index** is one its registry no longer lists, usually because
the author or the registry removed it. Valmin cannot offer updates for it and leaves it
out of **Update all mods**. It keeps working as installed.

### Disable a mod without removing it

To find out which mod is causing a problem, stop the server and choose **Disable** on
a mod. Valmin moves the mod's files out of the server into a `mods-disabled` folder
beside it, so the mod loader cannot load it. The mod's settings stay in place. Start
the server to test without that mod, then choose **Enable** to put the same files back.
A disabled mod is left out of **Update all mods** and the client manifest. Removing
it still removes the files it installed. Removing any mod keeps its config files in
`BepInEx/config`, so your settings return if you reinstall it.

A mod that another enabled mod needs cannot be disabled until that other mod is
disabled. A mod cannot be enabled while one of its dependencies is disabled.
BepInEx itself cannot be disabled; disable the mods instead.

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

## Compare two servers

Open **Compare** on a server and pick the other server under **Compare with**. The
page lists only what differs: the game build, launch and backup settings, installed
mods with their versions, registries and enabled state, and configuration files. Each
section also counts what matches. Expand a configuration file to see its changed
lines. Lines marked − come from the server you opened the page on, and lines marked +
come from the other server.

Comparing changes nothing on either server. It needs `instance.settings`,
`mods.list`, and `config.read` on both servers, and the list offers only servers
where you hold all three. The page address names the other server, so you can share
a comparison as a link.

## Send server commands

The **Console** on a server's overview shows its live log. To send commands, install
the `Tristan-ValheimRcon` mod from **Mods**. Valmin configures it in
`BepInEx/config/org.tristan.rcon.cfg` with port `2455` and a random password, unless
the file already sets them, and stores the password encrypted. The RCON port is not
published on the host.

Start the server, type a command under the console, and choose **Send**. The reply
appears only in your console. Limits:

- The server must be running.
- Viewers cannot send commands. Operators can send `save`, `kick`, `ban`, `unban`,
  `banned`, and `ping`. Administrators can send any command.
- A command is one line of at most 1,024 bytes.
- Each server accepts 30 commands a minute, in bursts of five.
- Every command is recorded in the audit log.

## Share access

Administrators create invitations under **Invites** and manage accounts under
**Users**, both in the header's **Administration** menu. A member needs a grant on
a server before they can access it. Use the server's **Panel access** page to assign
a role and extra permissions.

Public server listing and the public status page are separate settings. Enable
**Public status page** in server settings to share an unauthenticated status link.

## Read the audit log

Administrators open **Audit log** in the header's **Administration** menu. Each entry says who
did what to which server and how it ended: in progress, completed, failed or cancelled. Expand
an entry to see the exact changes (old and new value of each setting or config key), the job
behind it, and the IP address. Secret values, such as the game password, are recorded as
changed but never shown.

Filter by action, actor, server and date range. The filters are kept in the page address, so a
filtered view can be bookmarked or shared, and **Export CSV** downloads every entry that
matches. Names shown are those from when the action happened, and users and servers you have
since deleted stay in the filters. Scheduled runs are not entered, since nobody requested them;
they appear in the server's job history.

## Get notified

Open **Notifications** in the header's **Administration** menu. Under **Destinations**,
add a Discord or generic webhook and send a test notification. Every enabled
destination receives unexpected stops, new game builds and failed backups.

Under **Alert rules**, pick a condition, a server or **Every server**, and at least one
destination, then select **Add rule**. The rule sends an alert when the condition opens
and when it clears. Low disk is host-wide, so its rule always covers every server. Use the
switch to pause a rule, and the pencil button to change it. Deleting a destination removes it
from every rule, and a rule with no destinations left sends nothing.

Three conditions have thresholds: a crash loop is a number of stops within some minutes
(3 in 30 by default), a stuck job has run longer than some minutes (60), and stale backups
are older than a multiple of the schedule's interval, above 1 (2). Leave a field empty to
use the default.

Turn on **Quiet hours** to hold a rule's alerts during a daily window in a chosen
timezone. The window may cross midnight. Alerts still open when quiet hours end are sent
then; one that opens and clears inside the window is not sent.
