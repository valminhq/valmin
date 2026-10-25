# Use the panel

[Documentation](README.md) / Use the panel

This guide walks through the panel screen by screen. Backups, schedules, auto-stop
and power cuts are covered in [backups, restore, and upgrades](operations.md).

- [Find your way around](#find-your-way-around)
- [Create a server](#create-a-server)
- [Run a server](#run-a-server)
- [Change server settings](#change-server-settings)
- [Update the game](#update-the-game)
- [Manage worlds](#manage-worlds)
- [Manage mods](#manage-mods)
- [Edit mod configuration](#edit-mod-configuration)
- [Manage players](#manage-players)
- [Compare two servers](#compare-two-servers)
- [Save and restore a setup](#save-and-restore-a-setup)
- [Share a server as a template](#share-a-server-as-a-template)
- [Share access](#share-access)
- [Your account](#your-account)
- [Read the audit log](#read-the-audit-log)
- [Get notified](#get-notified)
- [Use the Discord bot](#use-the-discord-bot)

## Find your way around

### The server list

The start page lists every server you can access. Each card shows the server's state,
its join code for a crossplay server, the server name players see, the world, and the
UDP port pair. **Start**, **Stop** and **Restart** work from the card, so you do not
have to open the server for a quick action.

Problems that need a look appear as chips on the card, and host-wide problems such as
low disk space appear above the list. The line above the list says when these
conditions were last checked; **Refresh** checks again.

With more than six servers, a search box and a **State** filter appear. Filter by
**Running**, **Stopped** or **Needs attention**. The counter shows how many servers
match.

If the panel finds containers it created but has no record of, for example after
restoring an older database, it lists them above the servers. See
[recover an unclaimed server](#recover-an-unclaimed-server).

### Server pages

Opening a server shows its tabs. A tab appears only when your access reaches it.

| Tab               | What it holds                                                                |
| ----------------- | ---------------------------------------------------------------------------- |
| **Overview**      | Connection details, controls, alerts, resources, recent operations, console. |
| **Mods**          | Installed mods, the catalogue, and the list of mods players need.            |
| **Mod config**    | BepInEx and plugin configuration files.                                      |
| **Players**       | Player activity, accounts seen on the server, and the player lists.          |
| **Notifications** | The server's own channels and the rules that post about it.                  |
| **Backups**       | World backups, retention, world import, and the worlds on disk.              |
| **Saved setups**  | Named snapshots of mods, configuration and settings.                         |
| **Maintenance**   | Schedules, auto-stop, upcoming or skipped runs, and world tools.             |
| **Panel access**  | Who can use this server and what they can do.                                |
| **Settings**      | Names, password, connections, status page, gameplay, limits, and management. |

### Switch between servers

When the panel has more than one server, the server header has a **Switch server**
menu listing every server. Choose one to open the same section of that server, for
example **Backups**, when your access there reaches it. Otherwise its overview opens.
The rest of the address, such as a file name or a search, is dropped. With a single
server the menu is hidden.

### Header menus

The **Administration** menu groups its pages under four headings:

| Heading          | Pages                                                                                                                                                            |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **People**       | [Users](#create-a-user), [Invites](#invite-someone), [Players](#players-across-servers)                                                                          |
| **Integrations** | [Remote backups](remote-backups.md), [Notifications](#get-notified), [Discord bot](#use-the-discord-bot)                                                         |
| **System**       | [Audit log](#read-the-audit-log), [Power cuts](operations.md#stop-every-server-before-a-power-cut), [Diagnostics](troubleshooting.md#check-the-diagnostics-page) |
| **Advanced**     | [Encryption keys](operations.md#rotate-encryption-keys)                                                                                                          |

Each page is shown only to accounts that may use it, and a heading appears only when
the account can see something under it.

The account menu, named after your username, shows who you are signed in as, then
**Change password**, **Date and time**, **Report an issue** and **Sign out**.
**Report an issue** opens a new issue on the project's GitHub page in a new tab.

## Create a server

1. Select **New server** from the server list.
2. Enter a panel name, the server name players see, the world name, and the server
   password. The password must have at least five characters and must not appear
   inside the server or world name.
3. Choose public listing and crossplay.
4. Optionally change the gameplay settings and the memory limit, add mods, or
   import an existing world. Each of these is described below.
5. Submit the form and follow the progress. The first server downloads the dedicated
   server through SteamCMD; later servers reuse that download and copy it, which on
   most filesystems is still a copy of about 1 GB.
6. Start the server if you did not turn on **Start server after setup**. Wait for
   the panel to report it ready before connecting.

Connect using the host's address and the assigned base port, or the join code shown
for a crossplay server. The server password is separate from your panel account
password. To look it up later, see [look up or change the password](#look-up-or-change-the-password).

### Gameplay and resources

**Game version** picks the Steam branch the server installs: `public`, the current release,
or one of the older builds Steam keeps, listed with Steam's own description, such as
`default_old` (the previous release) or `default_preml` (the last build before Mistlands).
The list comes from the panel's last update check, so it holds only `public` until one has
run. **Password branch…** installs a branch Steam does not list, such as `public-test`: enter
its name and the password the game's developers publish with it. Players must choose the same
branch in Steam, under Valheim's **Properties**, **Betas**.

**World preset** picks one of the game's presets, or **Server default**. The list
holds the presets the game is known to accept; the game may accept others.

**World modifiers** works like the game's own world settings. Each rule (combat, death
penalty, resources, raids and portals) has a slider. Each switch turns on one world
option: no build cost, player-based raids, fire hazard, passive enemies or no map.
A slider at **Normal** uses the server default, or the preset's value if a preset is
chosen. Turn on **Manual** to type values instead of using the sliders. The panel does
not check typed values, so enter only values the game accepts. A saved value that no
slider position matches opens in Manual mode.

**Custom seed** generates the new world from a seed you choose. Turning it on adds the
`DeeJay-CustomSeed` mod to **Mods** and shows a **Seed** field that takes 1 to 10 letters
or digits. The panel writes the seed to the mod's config file,
`ZeroBandwidth.CustomSeed.cfg`, after the mods install and before the first start. If a
chosen modpack already includes the mod, the switch shows as on and the seed is optional;
left empty, the mod keeps its own default. The seed applies only to a world the server
generates, so the switch is unavailable when you start from an existing world. To change
the value later, edit `ZeroBandwidth.CustomSeed.cfg` in the config editor. It affects
only worlds generated after the change.

**Memory limit (MB)** caps the server's container. The form shows the minimum. Leave
generous headroom: a server that runs out of memory can be killed in the middle of a
save. The form also shows how often the game saves and how many of its own rolling
backups it keeps. Those are separate from the panel's backups and are not included in
them. Change the count later in the server's [Gameplay](#gameplay) settings.

### Add mods before the first start

Open **Mods** on the form to pick mods from the catalogue. They are installed before
the server first starts, which matters for mods that shape world generation.

### Start from an existing world

Open **Start from an existing world** and choose the world folder under
`worlds_local`, a ZIP of that folder, or both the `.db` and `.fwl` files of an older
save. Keep the page open until provisioning and import finish. The imported files
are renamed to the world name you entered. The server stays stopped during the import
and starts after it when **Start server after setup** is on. See
[import a world](#import-a-world) for the rules.

### Create a server from a template

Choose **New from template** on the server list to build a server from a shared
template, a template file or a template code that someone shared. See
[use a template](#use-a-template).

### Clone a server

Administrators can copy an existing server. Stop the source server, open its
**Settings**, and choose **Clone** under **Manage server**. Enter a panel name for
the copy and choose **Clone server**.

The copy gets the world, the installed game build, mods, configuration files, launch
settings and the server password. It gets its own ports, identity and data
directories, and stays stopped when it is ready. Users, access grants and the source
server's backups are not copied. After cloning, the two servers are independent.

Cloning never stops the source server for you; it is refused while the source runs.

### Recover an unclaimed server

A container that Valmin created but no longer has a record of, for example after
restoring an older `panel.db`, shows on the server list as **not claimed by any
server**. Administrators can choose **Review and recover** to bring it back under
management.

The page shows what the panel found: the container name, whether it is running, its
UDP ports, its game build, and whether it has mod loader files. Enter the panel name,
server name, world name, password and other launch settings exactly as the container
was created. Valmin checks them against the container and refuses values that do not
match. Recovery never stops, recreates, copies or changes the container or its files.

## Run a server

### Start, stop and restart

Use **Start**, **Stop** and **Restart** on the overview or the server list. A stop asks
the game to shut down and waits up to two minutes for it to save the world, so a stop
can take a while on a large world. Each action runs as an operation that you can follow
on the overview.

If **Back up when this server restarts** is on under [backup settings](operations.md#backup-settings),
every restart, including a scheduled one, makes a consistent backup between the stop
and the start.

### Restart notices

A changed setting does not reach a running server until it restarts. The panel shows
two different badges:

- **restart required**: launch settings or a new password changed something the
  container is built from. The server keeps running with the old values until it
  restarts.
- **pending restart**: saved mod configuration, mod changes or a restored setup are
  waiting for the next start. This one is informational and never raises an alert.

### The overview

The connection card at the top shows the port pair, join code for a
crossplay server, and the server password behind **Show**. On a running server,
**Check reachability** tests whether players outside your network can find the server (see
[check that friends can connect](#check-that-friends-can-connect)).

### Check that friends can connect

**Check reachability** on the connection card answers "my friends can't connect". It looks up
the host's public IP address, asks Steam whether a server is registered there, and sends the
server a query on its public address from the panel host. A server answers these queries, and
is listed by Steam, only while public listing is on, so the last two steps run only then. It
usually takes a few seconds and at most about twelve, and each person can run it five times a
minute. The card then shows one
of these:

- **The server answers at the address.** Players connect to the join address shown.
- **The server answers, but Steam does not list it.** The server registers with Steam some time
  after it starts; check again shortly.
- **The server did not answer.** The remedy lists the usual causes, and the check cannot tell
  them apart: the router does not forward UDP ports base and base+1 to this host, the host's
  firewall blocks them, the router lacks NAT loopback (it hides a working server from the panel
  host; ask a friend outside your network to join at the address shown), or the connection is
  behind carrier-grade NAT. The last applies when the router's own internet address differs
  from the public IP shown or is in 100.64.0.0/10 (100.64.0.0 to 100.127.255.255): no
  forwarding can work then, so turn on crossplay in the server's settings and share the join
  code instead.
- **Public listing is off.** The check shows the join address but cannot test it. Turn on public
  listing and restart the server to test it, or ask a friend outside your network to join at
  the address shown.
- **Crossplay.** With a join code, players join with the code, and the check does not test the
  direct address. Without one, the server has not logged its code yet; check again in a few
  seconds.
- **A restart is pending.** A setting changed since the server started, and the running server
  may not match it, so the check measures nothing. Restart the server, then check again.
- **Unknown.** The public IP lookup is turned off or failed; see
  [configuration](configuration.md#check-that-players-can-reach-a-server).

The check runs from inside the host, so it cannot see the router. A router without NAT loopback
can make a working server look unreachable. The opposite cannot be ruled out either: an answer
from the panel host does not prove the router forwards the ports or that a firewall in front of
the host allows them, and a Steam listing only shows that the server registered, not that
players can connect. Joining from a connection outside your network is the final test.

**Resources** shows CPU and memory, sampled every two seconds, and the current player
count while the server runs. Disk use is always shown, split into world, server files
and backups, with the free space left on the host. When free space drops below the
alarm level, the card says so in red: the game stops saving the world when the disk
fills and does not report it, so free space before playing further.

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

### Alerts on the overview

**This server needs a check** appears when an operation failed in a way that left the
server's state uncertain. Valmin holds the server's controls and names the operation
that failed, for example `game update`, followed by its error. **Check this server**
compares the server with Docker and sets it back to stopped or running, whichever is
true.

**The last stop was not confirmed** means the server exited without the panel seeing
the world save finish. The world is probably intact, but the stop cannot be confirmed
as clean. This alert looks only at the newest operation that reported whether the save
finished, so an older unconfirmed stop further down the list does not raise it.

The overview shows one red alert at a time: while a server needs a check, or its setup
did not finish, the unconfirmed-stop alert waits.

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

### Send server commands

The **Console** on a server's overview shows its live log. To send commands, install
the `Tristan-ValheimRcon` mod from **Mods**. Valmin configures it in
`BepInEx/config/org.tristan.rcon.cfg` with port `2455` and a random password, unless
the file already sets them. Anyone who can read the server's mod configuration can read
that password in the file. A cloned server gets a password of its own. The RCON port is
not published on the host.

Start the server, type a command under the console, and choose **Send**. The reply
appears only in your console. Limits:

- The server must be running.
- Viewers cannot send commands. Operators can send `save`, `kick`, `ban`, `unban`,
  `banned`, and `ping`. Administrators can send any command.
- A command is one line of at most 1,024 bytes.
- Each server accepts 30 commands a minute from people, in bursts of five. The panel's
  own commands, such as restart warnings and world tools, have a separate allowance.
- Every command is recorded in the audit log.

With the same mod, Valmin also warns players in chat before a
[scheduled run](operations.md#schedule-maintenance) or a
[power cut](operations.md#stop-every-server-before-a-power-cut).

## Change server settings

Open **Settings**. Changes collect in a bar at the bottom of the page, which counts
them; **Save changes** stores them and **Discard changes** drops them. Saved settings
reach the server on its next start. Accounts without permission to change settings
see them read-only.

### Names

**Panel name** is what the server is called in the panel. **Server name** is what
players see in the server browser. Both can be changed at any time, which is how you
rename a server. **World name** is the save file on disk and cannot be changed here.

### Look up or change the password

To look up the server password, open the server's overview and select **Show** in
the connection card, then copy it from there. Anyone who can view the server can
read it, and each read is recorded in the audit log.

To change it, enter a new one under **Server password** in **Settings** and confirm.
The panel cannot tell players the new password. Players already connected stay
connected, and the running server keeps the old password until it restarts.

### Connections

**List publicly** shows the server in the in-game community browser. **Crossplay**
lets players on other platforms find and join it; they join with the join code that
appears on the overview and the server list once the server has registered. Some
combinations of crossplay with other settings have not been tested, and the form
lists them when they apply. Crossplay does not make a mod compatible with console
clients.

### Public status page

Turn on **Publish the status page** to share a page that anyone with the link can
open without signing in. The link appears under the switch and has the form
`https://YOUR_PANEL/status/<server id>`.

The page shows the server's name, whether it is up or under maintenance, and how many
players are on it, with the time the count was observed. Two optional texts appear on
it: **Status page notice**, for example `Down for the update until 20:00`, and **How
to join**, for the address, where to get the password, and the mods players need. Both
are plain text.

Visitors can refresh the page or turn on automatic refresh, which checks again every
30 seconds while the tab is open. Public listing in the game and the status page are
separate settings.

### Gameplay

**World preset** and **World modifiers** work as on the
[creation form](#gameplay-and-resources). What a changed preset or modifier does to
an existing world has not been verified, so back up the world before changing them.

**Game backups kept** is how many rolling saves the game keeps beside the world, 1 to 50.
The game's default is 4. It applies on the next start. Restore one from
[Worlds on disk](#worlds-on-disk).

### Resource limits

Administrators set **Memory limit (MB)** and **CPU limit (cores)**. Leave the CPU
limit empty for no quota; a tight quota can slow a game that depends heavily on one
core. The new limits apply when the container is rebuilt on the next start.

### Delete a server

Administrators find **Delete this server** under **Manage server** at the bottom of
**Settings**. The server must be stopped. Type the server's name to confirm.

Deleting removes the container, the game installation with its mods and
configuration, the logs, and the server's settings in the panel. The world files under
`instances/<id>/worlds/` and the backup archives under `backups/<id>/` stay in the data
root. No server lists them again, and they cannot be attached to another server. Copy out
what you want to keep, for example to [import a world](#import-a-world) into another
server, and delete the folders when you no longer need them.

## Update the game

When Steam publishes a newer dedicated server build, the overview shows **A newer game
build is available** with the installed and the public build numbers. Otherwise it
shows the installed build and says it is current.

Administrators choose **Update the game** and confirm. Valmin backs up the world,
downloads the new build, replaces the server files, and leaves the server stopped so
you can check it before starting. The world itself is not touched by the update.

On a modded server, the update puts every installed mod and your configs back onto the new
build. Files a mod wrote under `BepInEx` while it ran, outside its own package files and
configs, are not carried over; the update's log lists them. A new game build can still break
a mod, and the panel cannot check that in advance. For
that reason nothing updates a modded server on its own: a
[scheduled game update](operations.md#schedule-maintenance) skips it and records the
skip.

Rebuilding the game runtime image does not download a new game build; only this action
does.

When a newer build is available, **Recent game patch notes** on the notice lists the
game's latest patch posts from Steam, newest first, each with a link to its post on Steam.
Steam does not say which patch a build number belongs to, so the list shows the latest
patches, not only the ones this update adds. If Steam's news cannot be reached, the notice
links to the game's news page instead. The panel asks for the notes only when you open the
list, and keeps the answer for an hour.

### Test an update on a copy

On a modded server, **Test an update on a copy** on the overview shows what an update would
do before anything on the server changes. It needs permission to clone, update and manage
mods, which administrators have. Choose whether to try the new game build, the pending mod
updates, or both, and choose **Test on a copy**.

Valmin then:

1. Copies the server, with its latest world, mods, configs, launch settings and password.
   The server may be running and keeps running. A running server's world is copied as of
   the game's last save, so it can lag behind by up to the save interval, 30 minutes by
   default.
2. Installs the new game build and the pending mod updates on the copy.
3. Starts the copy and reads what the mod loader said.

The copy is an ordinary server in the server list, named after the original with
**update test** added, and a number after that when another server already has the name.
It is not listed publicly and has crossplay off. It gets its own
port, which the card shows, so you can join it with a game client to try the update
yourself. It runs with the original's memory limit and half of its CPU limit, or half of the
host's cores when the original has no limit. The test is refused when the host has less
free memory than the copy may use. The copy stops itself after 10 minutes with nobody on
it; its **Settings** can change that. A server has one copy at a time.

The card then shows the copy's state, the game build before and after, and each mod
that moved to another version, with a **Changelog** under it (see
[Read what changed in a mod](#read-what-changed-in-a-mod)). Mods the loader said it could
not load are listed in red with the loader's own line. A copy whose loader wrote no log is
reported as not checked rather than as a success.

- **Apply to the server** stops the original with its world saved if it is running,
  backs up the world, installs the game build and mod versions the copy runs, starts the
  server again if it was running, and deletes the copy. It is available once the copy has
  started and its test ran to the end; a test that failed or was cancelled can leave the
  copy with only part of the update. Mods are installed at the versions the copy runs, not
  at whatever is newest then, and only where those are newer than what the server runs: if
  you updated the server by other means since the test, nothing older is put back. The game
  build comes from the download the test already made, so nothing is downloaded again; if
  that build has left the cache the apply fails instead of installing a different build.
  The apply can be cancelled only before it starts changing the server; after that the
  cancel is refused. If a step of the apply fails after the server was stopped, the server
  is left stopped, possibly on the new game build with some mods not yet updated, and the
  copy stays. Check the job's log, then start the server, or restore the backup the apply
  took.
- **Discard the copy** deletes it with its world and backups. The original is not touched.
  A running copy has to be stopped first.

A copy cannot itself be tested.

### Change the game version

Administrators choose the server's Steam branch under **Game version** on the
**Maintenance** page, in the same way as when creating a server. Saving changes no files;
the overview then shows the branch's build, and **Update the game** installs it, backing
up the world first. Switching works in both directions, to an older build or back to
`public`, and installed mods and their configuration are put back as in any update.

An older build may not load a world that a newer build has saved. If it does not, restore
the backup the update took, or switch back.

Steam does not publish the builds of a password branch, so the overview cannot say whether
a newer one exists. **Update the game** downloads the branch's latest build every time, and
no update alert fires for it.

## Manage worlds

The **Backups** page holds everything about the world: backups, retention, import, and
the worlds in the save directory. Backups and restore are described in
[back up or restore a world](operations.md#back-up-or-restore-a-world).

### Import a world

Bring a save from a single-player game or another server, either on the
[creation form](#start-from-an-existing-world) or, for an existing stopped server,
under **Import world** on **Backups**.

Choose the world folder under `worlds_local`, a ZIP of that folder, or both the `.db`
and `.fwl` files of an older save. Keep the complete folder for saves that include
chunk files. The files are checked before anything is written and renamed to the
server's world name. On an existing server, the current world is backed up first, so
an import can be undone from the backups list. Type the world's name to confirm.

The game keeps its own older saves, such as `.old` files. The importer refuses those
unless you turn on **Import an older game backup**, which restores the world to that
earlier save.

### Worlds on disk

Under the backup history, **Worlds on disk** lists every world in the server's save
directory with its size. The world the server loads is marked, and a world missing its
header or data file is marked incomplete. If the world the server is set to load is
not there, the page says so: backups refuse until the server creates it or you import
one under that name.

**Load this one instead** replaces the loaded world with another world from the list.
The game's own rolling saves are listed separately under **Game backups**, newest first,
each with **Restore this backup**. Either way the server must be stopped, and the current
world is backed up first. How many rolling saves the game keeps is set under
[Gameplay](#gameplay).

### Delete a world or start one over

Stop the server, then use **Delete** beside a world to remove it. Deleting the world
the server loads resets it: the server generates a new world the next time it starts.

The whole save directory is backed up before anything is removed, so a deletion can
be undone from the backups list. A restore brings back the worlds only. The archive
also holds the player lists, but a restore leaves the current ones as they are.

If the server was started outside the panel, for example with `docker start`,
imports, deletions and restores fail without changing the world. Stop it and retry.

## Manage mods

Open **Mods**. The **Installed** tab lists what the server loads, **Browse mods**
searches the catalogue, and **Player modpack** lists the mods players need on their
own computers.

Search the catalogue, choose **Install**, review the dependency plan, and apply it.
Mods change only on a stopped server, but you can install one while the server runs.
The install then waits in **Waiting for the server to stop** on the **Mods** page, and
you can remove it from there until it runs. Queued installs run once the server stops,
and the section then reads **Installing queued mods** until they finish. Updates you
queued together run as one update; other installs run one at a time. **Restart** does
this for you: it stops the server, runs the queued installs, and starts the server
again, even when one of them fails. **Update all mods** queues the same way. Removing
and turning mods on or off still need a stopped server.

If a mod would overwrite files on the server that no installed mod owns, such as a plugin
you copied in by hand, nothing is installed and Valmin lists those files. Choose **Keep
existing files** to install the mod without them; they stay as they are and are never
removed with the mod. Choose **Replace them** to let the mod overwrite them; they then
belong to the mod and are deleted when you remove it. A queued install that meets such
files stops; install that mod by hand to choose.

Saved mod changes mark the server **pending restart** until its next start. Check the
operation result after each change.

A mod the author has marked deprecated says so on its row. It may not work on the
current game build.

### Choosing a registry

The catalogue is fed by two registries, **Thunderstore** and **Hexium**, and the
buttons above the search box pick which one you are browsing. **All** shows both.

Each listing is labelled with its registry and its version is shown in that
registry's colour. A mod both registries publish appears twice, once per registry,
usually at different versions. That is not a duplicate; it is the choice of which copy
to install. The registries are run by different people, so the same name and version
number on each is not a guarantee of the same files.

A server installs any given mod from one registry at a time. Where a mod is already
installed from the other one, its row says so and offers no install; uninstall it
first if you want to switch. An update is only ever offered from the registry the
installed files came from.

Updating a single mod, or installing one that raises an installed package to a new
version (a newer BepInEx, for example), shows each changed package's current and new
version. On a server with a world, the job backs up the world first and keeps the
backup even if the update fails.

The catalogue refreshes when the panel starts and then on a timer. An admin can refresh
it sooner with the refresh button beside **Catalogue updated** on the **Browse mods**
tab.

If a registry is switched off in the [configuration](configuration.md#mod-registries),
it disappears from search and nothing new can be installed from it, but mods already
installed from it keep working and keep their details on this screen.

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
appears on the **Backups** page as a pre-update backup.

On a running server, **Update when stopped** queues the updates instead. Once the server
stops, or when you restart it, they run as one update the same way: one backup of the
world, then the whole set. An install queued ahead of them runs first, on its own.

A mod marked **not in the index** is one its registry no longer lists, usually because
the author or the registry removed it. Valmin cannot offer updates for it and leaves it
out of **Update all mods**. It keeps working as installed.

### Read what changed in a mod

For those who can manage mods, an update lists each package with a **Changelog** under it,
in the install dialog and in **Update all mods**. Opening it shows the `CHANGELOG.md` the new version ships, read from
the package the update would install, so the install does not download it again. The first
read of a large package waits for its download. A version without that file says it ships
no changelog. Thunderstore packages also link to their
page; Hexium's page address is not known, so its packages carry no link. The text is cut at
256 KiB. To try updates safely first, see
[Test an update on a copy](#test-an-update-on-a-copy).

### Update mods from a code

**Update from code** on the **Installed** tab makes this server's mods match a profile
code from r2modman or Gale. Paste the code and choose **Check code**. The list shows what
would change: **new** mods, **update**s and **downgrade**s, each with its registry. New mods
and updates are ticked; a downgrade is not, so tick it if you want that older version.
Ticking or unticking a row recalculates the list, including the dependencies the change
needs.

A mod stays in the registry it is installed from. If the code names another registry for an
installed mod, or asks for a version its registry does not have, the mod is listed under
**Left alone** with the reason, and nothing is taken from the other registry. A new mod comes
from the registry the code names, otherwise the one that served the code, otherwise any
enabled registry that has that version.

Mods on the server that are not in the code are never removed or changed. Locked mods,
disabled mods, the mods a modpack manages and the mod framework are left alone and listed.
The code's config files are not applied; use **New from template** to create a server with
them. A code from a player's computer may list mods that only run on players' computers, so
untick those.

The server needs its mod framework, which arrives with the first mod you install from
**Browse mods**; the framework is never installed from a code. The server must be stopped.
**Back up and apply** backs up the world, then makes all the
changes in one job. If any package fails, the job rolls back every package and keeps the
backup.

### Disable a mod without removing it

To find out which mod is causing a problem, stop the server and choose **Disable** on
a mod. Valmin moves the mod's files out of the server into a `mods-disabled` folder
beside it, so the mod loader cannot load it. The mod's settings stay in place. Start
the server to test without that mod, then choose **Enable** to put the same files back.
A disabled mod is left out of **Update all mods** and the player modpack.

A mod that another enabled mod needs cannot be disabled until that other mod is
disabled. A mod cannot be enabled while one of its dependencies is disabled.
BepInEx itself cannot be disabled; disable the mods instead.

### Remove a mod

Choose **Remove** on a stopped server. Valmin deletes exactly the files the mod
installed; the world is not touched. Two options in the dialog control what else goes:

- **Remove unused dependencies too** also removes the dependencies that no other mod
  still needs.
- **Remove its config files too** deletes the mod's configuration files, and those of
  the dependencies removed with it. The dialog names the files. Leave it off to keep
  your settings, so they return if you reinstall the mod.

### When a mod does not load

After the server starts, the **Mods** page shows what the mod loader reported.
If the loader says it could not load a mod, that mod's row turns red and shows the
loader's message, such as a missing dependency or an error from the mod's own
code. The alert at the top of the page names every mod that failed. **Not loading**
means the loader did not mention the mod at all. Check the loader log for that
mod's name.

### Tell players which mods they need

Mods that also run on players' computers have to match the server's versions. On the
**Installed** tab, label each mod **Server only**, **Client required** or **Client
optional** with the drop-down on its row.

The **Player modpack** tab then lists every mod labelled for clients, with the
dependencies they need, pinned to the versions the server runs. Unlabelled mods are
left out and named, so nothing is guessed. If a client mod needs one labelled
**Server only**, or one that is no longer in the catalogue, the tab names the conflict
and the list cannot be exported until it is fixed.

**Download client list** saves a profile file that r2modman, Gale and Thunderstore
Mod Manager can import. It carries package names and versions only. Those managers
resolve names against Thunderstore, so a mod installed from Hexium and published
nowhere else may not be found on import. Players can still install it by hand from
the registry's own page.

**Create profile code** uploads the same list to Thunderstore, or to Hexium when a mod
exists only there, and shows a code with a **Copy code** button. Players paste it into
r2modman or Gale instead of importing a file. Hexium codes work in Gale only. Uploading
publishes the package names and versions to the registry, and a registry may stop serving
a code after a while, so create a new one when a player cannot import it.

## Edit mod configuration

Open **Mod config**. The list shows the server's BepInEx and plugin configuration
files. Its search box keeps the files whose name or plugin name contains what you
type, ignoring case, and shows how many files match, for example `3 of 12 files`.
Adding `?q=` and some text to the page address opens the list already filtered.

Some mods keep a `.cfg` at the root of the server installation instead, usually to hold a
secret such as a bot token. The list shows such a file, marked **Server root**, when its
name starts with an installed mod's name, for example `FiresDiscordIntegration_BotToken.cfg`.
Shared setups and template codes never include these files.

If a new mod has no configuration file yet, start the server once so it can generate
one.

### The settings form

Opening a file shows its settings grouped by section, with the mod's own description
of each. Values get a matching control: a switch, a number, a slider for a range, or a
list for a fixed set of choices. On a wide screen, the section list on the left jumps
to a section, and **Filter settings** narrows the form to matching settings. A setting
changed from its default offers **Reset to default**.

When you are done, choose **Review changes**. The dialog lists every changed setting
with its old and new value, and nothing is written until you confirm. The rest of the
file is left as it was, comments included, and the version before the save is kept.
If a value is invalid, nothing is written and the form marks the setting.

### Compare with an earlier version

Above the form, **Compare with** picks a kept version of the file: **the original**,
as the mod first wrote it, or **before the last save**. The panel then lists the
settings that differ, marks each changed field, and offers to load one value or all
of them back into the form. **Compare as text** opens the same comparison line by
line.

### Edit as raw text

The **Raw text** tab edits the whole file as text, for changes the form cannot show,
such as comments or settings the mod does not describe. It has no validation. It
compares line by line against the chosen version. Save or discard form changes before
switching to it. Raw editing needs its own permission, `config.raw`.

If the file changed after you opened it, for example because the mod rewrote it,
nothing is saved. The panel shows the saved version so you can discard your edits or
overwrite the file with them.

### Edit while the server runs

You can edit configuration on a stopped or a running server. Edits to a running
server take effect when it restarts, so restart it once you have saved everything;
some mods pick changes up sooner. If a mod writes its old settings back when the
server stops, Valmin puts your saved values back before the next start.

Leaving a page with unsaved edits asks you to confirm first, so a mistyped link
does not discard them. Saving clears the prompt.

## Manage players

Open **Players**.

### Player activity

The chart shows the server's observed player count over the last 24 hours,
7 days, or 30 days. **Previous** and **Next** move between periods; **Back to live** returns
to the current period. The dates use your time zone. Hover or tap the chart to
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

### Seen on this server

Operators and administrators also see the accounts the server named in its log, with
each account's name, platform ID and when it was last seen. Seen means the account
reached the server, not that it played or was let in. **Copy ID** copies the ID for
the lists below. The server prints the same names and IDs in its log, so anyone who
can read the console can see them there.

When the server refused an account's latest attempt because of a wrong password, the
account shows **Refused: wrong password** with the time. The mark clears on the
account's next attempt. It has been confirmed on crossplay servers only.

#### Names

An account can have several names, and the ID is the only thing that identifies it.
A name shown plainly was printed beside the ID: by the server's own history when it
loads, or by a ValminEye join. A name shown as **maybe Troll · 2×** was matched by
timing. On a crossplay server the line that names a character does not name its account,
so each account that was waiting to spawn when a character appeared gets one count for
that name. The real owner collects a count every session; someone who only joined at the
same time collects them as long as the two keep joining together. A guessed name becomes
plain once it has at least three counts and no other account has half as many for the
same name. Two friends who always join together stay uncertain until one joins alone.
An account that connects and never spawns, such as one the server turns away, stays a
candidate for two minutes and can collect a few counts; they stay below the threshold
unless it keeps doing so.

A server without crossplay prints no line linking a joining player to their ID, so on
those servers only names from the server's history and ValminEye are shown.

### Players across servers

Administrators open **Players** in the header's **Administration** menu for every
account any server has seen, newest first, with its names and the servers it was seen
on. Search matches an ID or any name. Names and counts are the ones above, summed over
the servers.

### Player lists

Three lists control who may do what:

| List          | Effect                                                 |
| ------------- | ------------------------------------------------------ |
| **Admins**    | These players may use in-game administrator commands.  |
| **Banned**    | These players are refused when they try to join.       |
| **Allowlist** | When it has anyone on it, only these players may join. |

Enter one ID per line, bare or with a platform prefix, and choose **Save list**.
Comments already in a file are kept but not shown. Saving does not restart the server.
The game usually picks up the change while running; if it does not, use **Restart
server** on this page. If someone else changed the list while you edited it, the
panel shows the saved version and lets you discard your edits or overwrite it.

The lists are files in the server's save directory. A world restore leaves them as
they are.

## Compare two servers

Choose **Compare with another server** at the right of the server header, then pick the
other server under **Compare with**. The page lists only what differs: the game build,
launch and backup settings, installed mods with their versions, registries, locks,
enabled states, and client labels, and configuration files. Each section also counts
what matches. Expand a configuration file to see its changed lines. Lines marked − come
from the server you opened the page on, and lines marked + come from the other server.

Comparing changes nothing on either server. It needs `instance.settings`,
`mods.list`, and `config.read` on both servers. The header link appears only on servers
where you hold all three, and the list offers only servers where you hold all three.
The page address names the other server, so you can share a comparison as a link.

## Save and restore a setup

A saved setup is a named snapshot of a server's mods, configuration and settings,
kept on the same panel so you can go back after a change goes wrong.

Stop the server, then open **Saved setups**. The name field starts as **Working before
update**; change it if needed, then choose **Save setup**. You can link one consistent
world backup from this server. A linked backup stays outside retention and cannot
be deleted while a setup links to it.

A setup keeps launch and backup settings, Valmin-managed mods with their exact
registries, versions, locks, enabled states, and client labels, plus supported mod
configuration files: flat `.cfg` files up to 1 MiB each. Valmin keeps the
required package files, so a restore does not need the registry or its download
cache. Saving fails if a required package cannot be captured. Game build and world
name appear in the comparison but do not change on restore. The server password and
world files are excluded.

Select a setup to compare it with the current server. Review the restore preview,
then restore while the server is stopped. Valmin applies the saved mod set,
configuration, and settings together. If installation fails, it rolls back the
changes. The server stays stopped; start it after checking the job result.

To recover the world too, restore the linked backup separately on **Backups**. World
restoration replaces world files, so choose it only when needed. Delete a setup to
release its package references and its link to the backup. More detail is in
[recover a saved setup](operations.md#recover-a-saved-setup).

## Share a server as a template

A template recreates a server's setup on any Valmin panel. Both forms are under
**Share as a template** on the server's **Settings** page.

**Download template file** saves the launch settings, the mods at their installed
versions and every config file. It carries no password and no world. A mod's config
can hold a key or a webhook, so read the file before you share it.

A template code is a short text that recreates the server's mod setup. Choose **Create
template code**, then **Copy code**. Codes start with `valmin1:`.

The code carries launch and backup settings, enabled mods pinned to their versions and
registries, and the settings changed in the panel's config editor. Dependencies, including a
modpack's members, are left out and added again on import. The code excludes the password, the
world, disabled mods, mods installed outside Valmin, and changes made to config files outside
the panel. The summary under the code lists what was left out.

Under the code, each config file with changed settings is listed with those settings. A
setting whose name looks like a password, a token, a key or a webhook is marked **left out**:
the code skips it unless you turn on **Include secret settings** for its file. A setting
the code carries is marked **looks secret** when its name or value looks like one, such as a
URL or a bot token, and a warning names its file. Anyone with the code can read these values.
Turn off **Include** for a file to leave the whole file out of the code; keep secrets in only
when the code goes to your own panels or people you trust.

### Use a template

Choose **New from template** on the server list. **Shared templates** lists the
templates in the [Valmin presets](https://github.com/valminhq/valmin-presets)
repository. Filter them by name or description, then choose **Use** to check one.

The panel downloads the repository as an archive from GitHub and keeps it for an hour,
or until you choose **Refresh**. An unchanged repository is not downloaded again. If
the download fails, the page shows the error and keeps the last templates it read. A
template file that the panel cannot read is listed under the templates with the reason.

You can also choose the file under **Template file**, or paste a code under **Or paste
a code** and choose **Check code**. The code can be a template code from another panel or a
profile code from r2modman or Gale; the panel downloads a profile from Thunderstore, then
Hexium. A profile creates a server named after it, with its enabled mods and the settings
in its config files, apart from `BepInEx.cfg`; disabled mods are left out. Mods that only run on a player's computer
are installed on the server too, so check the list in the preview.
The preview lists the server settings, the pinned mods and the config files. For a code,
each config file is shown with the settings it changes, and a setting that looks like a
token or a webhook is marked **looks secret**. A mod no longer in the catalogue is marked,
and problems that block the template are listed at the top. Choose a panel name and password, then choose **Create server**.

Valmin downloads the game, installs each pinned mod in turn, then writes the
configuration. A code's settings are applied over the config files the mods create; a
file's config files are written as they are.

### Publish a template

A template repository holds one directory per template under `templates/`. Each
directory has a `template.yaml` and, optionally, a `README.md` for readers. The
panel reads only `template.yaml`:

```yaml
name: Vanilla QoL
description: |
  Vanilla progression with less inventory juggling.
code: valmin1:...
```

`name` and `code` are required, and `description` is optional. The directory name is
the template's id: lowercase letters, digits and dashes. To add a template to Valmin
presets, create its code under **Share as a template**, add the directory, and open
a pull request.

## Share access

Administrators manage accounts under **Users** and **Invites**, both in the header's
**Administration** menu. A member needs a grant on a server before they can access it.

### Roles and permissions

A panel account is a **Member** or an **Administrator**. An administrator reaches every
server and manages servers, users, access, settings, schedules and the audit log. A
member uses only the servers they are given access to.

On each server, a member's grant has a base access and optional extra capabilities:

| Base access  | Allows                                                                                                                         |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------ |
| **Viewer**   | See the server, its console, resource use, backups, mods and configuration.                                                    |
| **Operator** | Everything a viewer can, plus start, stop, restart, make and download backups, manage player lists, and send console commands. |

| Extra capability       | Allows                                                                                            |
| ---------------------- | ------------------------------------------------------------------------------------------------- |
| `mods.manage`          | Install, update and remove mods, which means running third-party code.                            |
| `config.edit`          | Change mod settings through the settings form.                                                    |
| `config.raw`           | Write mod configuration files as raw text, and everything `config.edit` allows.                   |
| `backups.restore`      | Replace the live world from a backup, delete backups, and set how many are kept.                  |
| `world.import`         | Upload a world and replace the live one.                                                          |
| `instance.settings`    | Change the server name, password, discovery, crossplay and world rules.                           |
| `notifications.manage` | Add this server's own channels and choose what the panel posts about it, also to shared channels. |

Some actions are for administrators only and cannot be granted: creating, cloning,
recovering and deleting servers, resource limits, game updates, saved setups, world tools,
schedules that cover the whole panel, users, invites, access, panel settings and the
audit log.

### Create a user

On **Users**, enter a username and choose a panel role. Two lines beside the role
choice say what each role can do, and one line under each existing user's role choice
does too.

Valmin generates the password and shows it once. After you create a member, that panel
also lists every server as a link to its **Panel access** page with the new person
already chosen, or says that no servers exist yet. After you create an administrator, it
says administrators reach every server and need no server access. Resetting a password
shows only the new password. Only the owner, the account created at first-run setup, can
reset the owner's password.

Making someone an administrator, disabling their sign-in and resetting their password each
ask you to confirm first, and each ends that account's sessions. Changing an administrator
back to a member, and enabling sign-in again, apply at once.

Usernames are case-sensitive, so `Alex` and `alex` are different accounts. The audit log
records the account itself, not only its name.

### Invite someone

Instead of creating the account yourself, open **Invites** to create a single-use link
with which someone chooses their own username and password. Pick the **Initial server
access**, or **No server yet** for a member with an empty dashboard, then the base
access and any extra capabilities, and choose **Create invite**.

The link and its code are shown once. Copy one and send it to the person. An invite
expires after seven days by default; the expiry is shown when it is created. The
person opens the link, chooses a username and a password of at least eight characters,
and is signed in.

**Invite history** lists every invite with who issued it and whether it is pending,
redeemed, revoked or expired, with a filter for each. **Revoke invite** cancels a
pending one.

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

## Your account

### Change your password

Open the account menu, named after your username in the header, and choose **Change
password**. Enter your current password, the new one, and the new one again. The
browser checks that the last two match before it sends anything, and the new password
needs at least eight characters. The page is `/account/password`.

The account you are using stays signed in. Every other place the account is signed in,
in other browsers or on other devices, is signed out. Attempts are limited to five a
minute, whether or not the current password was right. An administrator resetting your
password from **Users** signs you out everywhere, including this browser. If you cannot
sign in at all, see [reset an account password](troubleshooting.md#reset-an-account-password).

### Set your time zone and time format

Open the account menu and choose **Date and time**. Pick a zone from the list, such as
`Europe/Berlin`, and save. The panel then shows times in that zone, and new schedules run
in it. Existing schedules keep the zone they were created with. The page is
`/account/date-time`.

Under the zone, **Clock** chooses a 24-hour or 12-hour clock and **Date** chooses day, month,
year (`31/12/2026`), month, day, year (`12/31/2026`) or year, month, day (`2026-12-31`).
Save them with **Save format**. Each one left on **Follow my browser** uses your browser's
language settings.

The zone and format are saved on your account, so they apply in every browser. Until you
choose one, the panel uses the zone your browser reports. Browsers with fingerprinting
protection, such as Firefox with resist fingerprinting, LibreWolf or Tor Browser, report
`Atlantic/Reykjavik` or `UTC` instead of your real zone; set it here to get local times.
**Follow my browser** clears the choice.

Schedules follow daylight saving: one set to 04:00 in `Europe/Berlin` runs at 04:00 Berlin
time all year. A run inside the hour a clock change skips is missed that day, and one inside
the hour it repeats runs twice. The schedule editor warns when the chosen time falls in such
an hour, which is 02:00 to 03:00 in most of Europe.

## Read the audit log

Administrators open **Audit log** in the header's **Administration** menu. Each entry says who
did what to which server and how it ended: in progress, completed, failed or cancelled. Expand
an entry to see the exact changes (old and new value of each setting or config key), the job
behind it, and the IP address. Secret values, such as the game password, are recorded as
changed but never shown.

Filter by action, actor, server and date range. The filters are kept in the page address, so a
filtered view can be bookmarked or shared, and **Export CSV** downloads every entry that
matches. If the panel cannot read the whole log, the download fails rather than stopping
short. Names shown are those from when the action happened, and users and servers you have
since deleted stay in the filters. Scheduled runs are not entered, since nobody requested them;
they appear in the server's [operations](#operations). **View in the audit log** on that list
opens this page filtered to the server.

## Get notified

Valmin posts to **channels**: Discord or generic JSON webhooks. A **rule** says which event
to post, to which channels, and how the message reads.

A channel is either **shared** or belongs to one server. Shared channels are managed by
administrators under **Notifications** in the header's **Administration** menu, and every
server's rules can post to them. A server's own channels are managed on its
**Notifications** tab, and only that server's rules can post to them.

Rules follow the same split. **Rules for every server**, on the Administration page, post
about any server, including servers added later, and about the host. A server's own rules,
on its tab, post only about that server. The Administration page also lists each server with
its number of rules and own channels; select one to open its tab.

The tab appears for administrators and for members granted `notifications.manage` on that
server. Such a member can add the server's own channels and rules and post to the shared
channels, but cannot change the shared channels or the rules for every server: those are
listed on the tab read-only, under **Rules for every server, which also post about this one**.

### Add a channel

Under **Channels**, enter a name, choose the receiver and paste the webhook URL, then select
**Add channel** and **Send test notification**. A Discord webhook URL is a password: the
panel stores it encrypted and never shows it again. It must be an `https://` address on the
public internet. Names are unique among the shared channels and among each server's own.

Three alerts are built in and need no rule: a server that stops unexpectedly, a failed
backup and a new game build. **Also post built-in alerts** on a channel decides whether they
reach it. A shared channel gets them for every server, a server's own channel for that
server, and a new game build reaches shared channels only. A channel that a rule already
sends the same news to gets it once.

The switch on the right pauses a channel. A paused channel receives nothing, including
messages that were still waiting to be sent when you paused it. Deleting a channel removes
it from every rule.

### Add a rule

Select **Add rule** and choose the event:

| Group                  | Event                                 | Sent when                                                            |
| ---------------------- | ------------------------------------- | -------------------------------------------------------------------- |
| Server status          | **Server started**                    | Valmin started or restarted a server and its world loaded.           |
| Server status          | **Server stopped**                    | Valmin stopped a server: by hand, by auto-stop or for a power cut.   |
| Server status          | **Power cut soon**                    | A planned power cut is close: players are being warned. Administration page only. |
| Problems               | **Crash loop**                        | A server is crashing repeatedly.                                     |
| Problems               | **Server error**                      | A server is in an error state and needs a check.                     |
| Problems               | **Unclean stop**                      | A server stopped before its world finished saving.                   |
| Problems               | **Job failed**                        | A job failed and nothing has retried it.                             |
| Problems               | **Job stuck**                         | A job has been running unusually long.                               |
| Maintenance            | **Update available**                  | A new game build is available.                                       |
| Maintenance            | **Restart required**                  | A change is waiting for a restart.                                   |
| Maintenance            | **Backups stale**                     | Scheduled backups are not producing archives.                        |
| Maintenance            | **Low disk**                          | The host is running out of disk space. Administration page only.     |
| Players, Bosses, Raids | [Game events](#game-events-valmineye) | What happens in the game, reported by ValminEye.                     |

Then set up the rule:

- **Post to**: the channels. With only one channel, it is ticked for you. On a server's tab,
  shared channels are marked **shared**.
- **Thresholds**, for three events: a crash loop is a number of stops within some minutes (3
  in 30 by default), a stuck job has run longer than some minutes (60), and stale backups have
  missed a number of the schedule's runs, above 1 (2), counted on the schedule's own days. Leave a field empty to use
  the default.
- **Message**, filled with the default, beside a preview of the Discord post. A name in
  braces, such as `{world}`, is replaced by that value of the event, and the buttons under the
  box insert one where the cursor is. Every message can use `{server}` (the server's name in
  Valmin) and `{event}` (the headline, such as **The server keeps crashing**). A line whose
  every name has no value, such as a join code on a server without crossplay, is left out.
  `**bold**` works as in Discord. **Use the default** puts the default message back. A message
  is at most 1000 characters.
- **Timezone for times in the message**, for events that show a time. Left empty, times are
  in UTC.
- [Message style](#message-style) and [Quiet hours](#quiet-hours).

**Send test** posts the message to each ticked channel now, filled with the preview's sample
values, as a test notification, so you can see it in Discord before saving. Each person can
send five test messages a minute, counting both rule and channel tests. **Add rule**
saves it. Closing the dialog by clicking outside it or pressing Escape keeps your changes
open; **Cancel** discards them.

Add as many rules as you like, including several for the same event: for example, a short
**Player joined** line to one channel and a styled card to another. A channel named by two
rules for the same event gets one message.

Each rule in the list shows where it posts and its settings in brief. Its switch pauses it;
nothing is deleted. **Deliveries** shows what it has sent, **Duplicate** opens a copy to save
as a new rule, the pencil edits it, and the bin deletes it.

A problem or maintenance event is sent when it starts. When it clears, Valmin edits that same
Discord message: it turns green and its title reads **Resolved after 23 minutes: …**. If the
first message never arrived, or was deleted in Discord, the resolution is posted as a new
message instead. Server status events are sent each time they happen.

**Server started** waits until the server has loaded its world and, on crossplay, reports
its join code, so players can join as soon as it arrives (after 15 minutes without a code,
it is sent anyway). A server that stops before then is not reported as started. **Server
stopped** says why: who stopped it, **No players for N minutes**, or **Planned power cut**.
A server that stops on its own, or that Docker restarted without Valmin, is reported as
stopped unexpectedly instead.

**Recent deliveries**, at the bottom of both pages, lists every attempt, newest first, and
can be filtered by channel and status. On a server's tab it lists only messages about that
server.

### Message style

**Message style** sets how a message looks in Discord:

- **One line**: a card titled with the server and the event, `Midgard · Player left`, with
  your message below.
- **Message only**: a card with just your message.
- **Plain text**: your message as an ordinary chat line, without a card. Add `{server}` or
  `{event}` to the message if readers need them.

For a card, **Link to the server** makes the title a link to the server's page in Valmin
(when `server.external_url` is set), **Show the time** adds the time under the card, and
**Own colour** replaces the event's colour on the card's edge.

### Quiet hours

Turn on **Quiet hours** to hold a rule's messages during a daily window in a chosen
timezone. The window may cross midnight. Alerts still open when quiet hours end are sent
then; one that opens and clears inside the window is not sent. Server status events are not
sent during quiet hours at all.

### Game events (ValminEye)

[ValminEye](https://github.com/valminhq/valmin-eye) is a server plugin that reports what
happens inside the game: who joined and left, boss fights, the first time each boss is
defeated, and raids. It opens no port, and players need nothing on their side.

Game events are rules like any other: add one on a server's **Notifications** tab for that
server, or on the Administration page for every server. Once a server has a game event rule,
its tab says whether the server has ValminEye. **Install ValminEye** opens the server's mod
search for it; restart the server afterwards. Once it is installed, **Plugin settings** opens
its config, `io.valmin.eye.cfg`, where each event and each optional field can be turned off.

| Group   | Event             | Posted when                                                | Fields                                                          |
| ------- | ----------------- | ---------------------------------------------------------- | --------------------------------------------------------------- |
| Players | **Player joined** | A player's character enters the world.                     | `player`, `id`                                                  |
| Players | **Player left**   | A player quits, loses connection, or is kicked.            | `player`, `id`                                                  |
| Bosses  | **Fight started** | A boss notices players. By default only undefeated bosses. | `boss`, `key`, `first`, `day`, `players`                        |
| Bosses  | **Fight ended**   | The fight ends: `kill`, `wipe` or `gone`.                  | `boss`, `key`, `first`, `outcome`, `day`, `duration`, `players` |
| Bosses  | **First defeat**  | A boss is defeated for the first time in this world.       | `boss`, `key`, `day`, `duration`, `wipes`, `players`            |
| Raids   | **Raid started**  | A raid begins near players.                                | `name`, `raid`, `players`                                       |
| Raids   | **Raid ended**    | The raid is over.                                          | `name`, `raid`, `players`                                       |

`{players}` lists the players as `Alex (2 deaths), Bo`, and `duration` is in seconds. A field
the plugin's config turns off is left empty. Formatting characters in a field, such as `*` or
`[`, are shown as written, so a player's name cannot add bold text or a link to the post. A
player's joins and leaves on one server are posted at most once a minute, so a player stuck
in a reconnect loop does not flood the channel.

## Use the Discord bot

The Discord bot lets people check and start servers from Discord with `/status`
and `/start`. Everyone in a linked channel can use both commands, so link only
the servers you are happy for them to start. Bot admins can also plan
[power cuts](operations.md#stop-every-server-before-a-power-cut) with `/shutdown`.

1. In the Discord Developer Portal, create an application, add a bot, and copy
   its token. Turn off **Public Bot** so only you can invite it.
2. Open **Discord bot** in the header's **Administration** menu, paste the token,
   turn on **Run the bot**, and select **Save Discord settings**.
3. Once the page shows **Connected**, use **Invite the bot to a Discord server**.
4. Turn on **Developer Mode** in Discord's advanced settings, then right-click
   your Discord server (and a channel, if you want only that channel) and copy
   its ID. Under **Links**, select **Add link**, paste the IDs, tick the
   servers, and save.

Add one link per Discord server or channel; each sees only its own servers. A
channel's own link wins over its Discord server's, and a thread follows its
channel. Turn off **Allow /start** on a link to make it status-only. When the
last link to a Discord server is removed, the bot takes its commands out of that
server the next time it connects.

`/status` lists each linked server as online with its player count, starting,
stopping or offline. A list too long for one Discord message ends with how many
servers it left out. `/start` starts a stopped server; with several linked
servers, pick one from the suggestions. The reply changes to say when the server
is online, or that it failed; the reason stays in the panel's job history. Together with
[auto-stop](operations.md#stop-a-server-when-nobody-plays), players can start
an empty server themselves when they want to play.

The bot connects out to Discord, so nothing new listens on the host and no port needs
to be opened.

### Bot admins

Under **Bot admins**, list the Discord users or roles who may use `/shutdown`, one
ID per line: right-click a member or a role and copy its ID. Choose the time zone the
times they type are read in, then save.

- `/shutdown add time:14:00` plans a power cut at the next 14:00, or give a date as
  `2026-10-09 14:00`. Every server on the panel stops before it, not only the linked ones.
- `/shutdown list` lists the planned power cuts in each reader's own time zone.
- `/shutdown cancel` cancels one; pick it from the suggestions.

Anyone else who tries `/shutdown` is refused. A role ID lets every member of that role
plan a power cut for every server, so whoever can assign roles in the Discord server
decides who that is. List user IDs instead when that is too broad. The audit log records the power cut as
`shutdowns.create` or `shutdowns.delete` by `Discord: <name> (<user id>)`.
