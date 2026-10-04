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
be undone from the backups list. A restore brings back the worlds only. The archive
also holds the player lists, but a restore leaves the current ones as they are; see
[back up or restore a world](operations.md#back-up-or-restore-a-world).

If the server was started outside the panel, for example with `docker start`,
imports, deletions and restores fail without changing the world. Stop it and retry.

## Manage mods and configuration

Open **Mods** to search the catalogue, review the dependency plan, and apply changes.
Use **Mod configuration** to edit BepInEx and plugin configuration. Stop the server
before editing files. If a new mod has no
configuration file yet, start the server once so it can generate one.

The list on **Mod configuration** has a search box. It keeps the files whose name or
plugin name contains what you type, ignoring case, and shows how many files match, for
example `3 of 12 files`. Adding `?q=` and some text to the page address opens the list
already filtered.

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

## Switch between servers

When the panel has more than one server, the server header has a **Switch server**
menu listing every server. Choose one to open the same section of that server, for
example **Backups**, when your access there reaches it. Otherwise its overview opens.
The rest of the address, such as a file name or a search, is dropped. With a single
server the menu is hidden.

## Compare two servers

Choose **Compare with another server** at the right of the server header, then pick the
other server under **Compare with**. The page lists only what differs: the game build,
launch and backup settings, installed mods with their versions, registries, locks,
enabled states, and client tags, and configuration files. Each section also counts
what matches. Expand a configuration file to see its changed lines. Lines marked − come from the server you
opened the page on, and lines marked + come from the other server.

Comparing changes nothing on either server. It needs `instance.settings`,
`mods.list`, and `config.read` on both servers. The header link appears only on servers
where you hold all three, and the list offers only servers where you hold all three.
The page address names the other server, so you can share a comparison as a link.

## Save and restore a setup

Stop the server, then open **Saved setups**. The name field starts as **Working before
update**; change it if needed, then choose **Save setup**. You can link one consistent
world backup from this server. A linked backup stays outside retention and cannot
be deleted while a setup links to it.

A setup keeps launch and backup settings, Valmin-managed mods with their exact
registries, versions, locks, enabled states, and client tags, plus supported mod
configuration files: flat `.cfg` files up to 1 MiB each. Valmin preserves the
required package files, so a restore does not need the registry or its download
cache. Saving fails if a required package
cannot be captured. Game build and world name appear in the comparison but do not
change on restore. The server password and world files are excluded.

Select a setup to compare it with the current server. Review the restore preview,
then restore while the server is stopped. Valmin applies the saved mod set,
configuration, and settings together. If installation fails, it rolls back the
changes. The server stays stopped; start it after checking the job result.

To recover the world too, restore the linked backup separately on **Backups**. World
restoration replaces world files, so choose it only when needed. Delete a setup to
release its package references and its link to the backup.

## Follow a server's activity

### Player activity

Open **Player access** to see the server's observed player count over the last 24 hours,
7 days, or 30 days. **Previous** and **Next** move between periods; **Back to live** returns
to the current period. The dates use your browser's timezone. Hover or tap the chart to
inspect an interval, or focus it and use the arrow keys. **View observations** lists the
exact intervals and their durations.

The summaries show the peak count, average concurrent count, time with at least one
player, and observed player-hours. Average and player-hours weight each count by how
long it lasted. **Observation coverage** shows how much of the selected period the
panel could measure. Gaps marked **not observed** are excluded from the summaries;
they do not mean that the server was empty. The panel retains 30 days of observations,
so a period without a retained starting count begins with unknown coverage.

The live period refreshes while the page is visible. **Now** comes from the live stats
feed and shows **unknown** when its reading is missing or stale. The count history
does not identify individual players or prove how long any one person played.

### Operations

The **Operations** card on a server's overview lists its recent operations, newest
first: the ten latest, with **Show older operations** loading ten more each time. Each row
names the operation, how it ended, and how long ago it ran. Expand a row for when it
ran, who started it (a person, **Schedule** for a scheduled run, or **Panel** for system
work or a user since deleted), how long it took, its outcome and message, and, for mod
operations, the packages it was asked to change with their versions where known. A failed
operation also shows its error and error code. **No error details recorded.** appears
only for a failed operation that has neither an error nor a message.

Under a failed operation, a link points to where to look next, when you hold the
permission that screen needs:

| Failed operation                 | Link to                     | Permission needed |
| -------------------------------- | --------------------------- | ----------------- |
| Start or restart                 | The console on the overview | `console.read`    |
| Mod install, uninstall or toggle | **Mods**                    | `mods.list`       |
| Backup or restore                | **Backups**                 | `backups.list`    |
| Game update                      | The game update notice      | `instance.update` |

Accounts that may read the audit log also get **View in the audit log**, which opens the
audit log filtered to this server.

**This server needs a check** names the operation that failed in words, for example
`game update`. **The last stop was not confirmed** looks only at the newest operation
that reported whether the world save finished, so an older unconfirmed stop further down
the list does not raise it.

### Search and copy the console log

The console has a search box above the log. It matches case-insensitively, as plain
text, among the lines the browser holds: at most 5,000 rows, which include the startup
segment of up to 500 rows that the browser never trims. The counter counts matching
lines, not occurrences within a line. **Enter** moves to the next match and
**Shift+Enter** to the previous one, wrapping at both ends, and **Escape** clears the
search. Stepping to a match pauses auto-scroll; choose **Follow latest** to resume it.
Search matches log lines only, never the panel's own notices about dropped lines, a
rotated buffer, or a recorded log.

**Copy selected text** copies the text you selected in the log, with the timestamps as
shown. It is disabled while nothing in the log is selected. Over plain HTTP the browser
refuses clipboard writes, and the button says so. The log draws only the rows in view,
so scrolling far enough that the selected rows leave the screen drops the selection.

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
a server before they can access it.

### Create a user

On **Users**, enter a username and choose a panel role. A **Member** uses only the
servers they are given access to, at the level each grant allows. An **Administrator**
reaches every server and manages servers, users, access, settings, schedules, and the
audit log. Two lines beside the role choice say this, and one line under each existing
user's role choice does too.

Valmin generates the password and shows it once. After you create a member, that panel
also lists every server as a link to its **Panel access** page with the new person
already chosen, or says that no servers exist yet. After you create an administrator, it
says administrators reach every server and need no server access. Resetting a password
shows only the new password.

### See which servers a user reaches

Each user on **Users** has a **Server access:** line. A member's line lists their
servers with the base role of each grant, for example `Alpha (operator), Beta (viewer)`.
An administrator reads **Every server (administrator)**. A member with no access reads
**No server access yet**, followed by a link to every server. Each server name links to
that server's **Panel access** page with the person already chosen. An expired grant does
not count as access.

The page reads every server's grants, in parallel, when it opens and again after each
change, and only administrators can read them. If one server's grants cannot be read,
the other servers still show, with a note naming the one that failed. If the server list
cannot be loaded, the note reads **Server access could not be loaded.** and members show
no summary line.

### Give access on a server

Use the server's **Panel access** page to assign a role and extra permissions.
**People with access** lists explicit grants only. Each grant shows:

- **Base access**, **Viewer** or **Operator**, with a sentence saying what it allows.
- **Extra capabilities**, each named in words with what it risks beneath it. Hover a
  name to see the underlying action.
- **Effective access**, one sentence combining the two.

**Unsaved changes** shows beside **Save access** while a grant differs from what is
saved. Saving, overwriting, revoking, or adding one person leaves everyone else's unsaved
edits alone. Only opening the page, switching to another server, and **Discard all access
edits** in a conflict notice reset every grant.

**Give access** offers the members who have no grant on this server, with the same base
access, extra capabilities, and effective access. A link from **Users**, which adds
`?user=` and the person's id to the page address, opens the page with that person
already chosen, provided they are a member with no grant here. Otherwise the parameter
is ignored and the first available person is chosen.

Administrators are not in the picker. They appear under **Administrators**, a read-only
list of enabled administrators, which says they can use every server without a grant.
Disabled administrators are not listed. A grant that still exists for someone who is now
an administrator stays under **People with access**, with a note that it changes nothing.

Public server listing and the public status page are separate settings. Enable
**Public status page** in server settings to share an unauthenticated status link.

## Change your password

Open the account menu, named after your username in the header, and choose **Change
password**. Enter your current password, the new one, and the new one again. The
browser checks that the last two match before it sends anything, and the new password
needs at least eight characters. The page is `/account/password`.

The account you are using stays signed in. Every other place the account is signed in,
in other browsers or on other devices, is signed out. Attempts are limited to five a
minute, whether or not the current password was right. An administrator resetting your
password from **Users** signs you out everywhere, including this browser.

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
they appear in the server's [operations](#operations). **View in the audit log** on that list
opens this page filtered to the server.

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

## Use the header menus

The **Administration** menu lists **Users**, **Invites**, **Audit log**,
**Notifications**, and **Diagnostics**, each shown only to accounts that may use it.
**Encryption keys** sits under an **Advanced** heading at the bottom of the menu. The
heading appears only when the account can see something under it.

The account menu, named after your username, shows who you are signed in as, then
**Change password** and **Sign out**.
