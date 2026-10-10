# Back up, restore, and upgrade

[Documentation](README.md) / Back up, restore, and upgrade

## Back up or restore a world

Open **Backups** on a server. Two buttons make a backup now:

- **Stop and back up** makes a consistent archive. Valmin stops a running server,
  waits for the world save to finish, makes the archive, and starts the server again.
  The server is offline for the whole backup. A server that was already stopped stays
  stopped.
- **Back up without stopping** is best-effort: an active save can produce an
  incomplete archive that may not restore. Use it only when you cannot allow downtime.

Each archive in the history shows when it was made, the world, whether it is
**Consistent** or **Best-effort**, why it was made (manual, scheduled, pre update,
pre restore or pre import), and its size. An archive that retention will remove at the next
prune is marked **Pending deletion**. **Download** saves an archive to your computer,
and **Delete** removes it after you type the server's name. With
[off-host copies](remote-backups.md) configured, **Upload now** sends an existing
archive to the remote destination. Downloading needs operator access; deleting needs
`backups.restore`.

Above the backup buttons, **Newest backup** and **Newest consistent backup** give the
time and age of each, so you can see at a glance what you could recover. When the newest
backup is consistent, the page says so once. When none of the loaded backups is
consistent, an amber warning says so. While older backups remain unloaded it reads
**None among the backups loaded so far**, so use **Load older backups** to look further.

The history lists the most recent archives. Use **Load older backups** below it to
reach the rest of what retention has kept.

### Backup settings

**Backup settings** beside the history sets retention: how many **consistent** and how
many **best-effort** backups to keep. The two counts are separate, so a run of
best-effort copies never pushes out your consistent ones. A count of `0` keeps all
backups of that type. Snapshots taken before a restore, import, or update have their own
limit, equal to the consistent count. Older backups are deleted after the next backup or
scheduled cleanup.

**Back up when this server restarts** makes a consistent backup on every restart,
including scheduled ones, between the stop and the start. The restart waits for it, so
large worlds add a few minutes of downtime. If the stop did not confirm the world save,
no archive is taken and the operation says so.

To back up on a schedule, see [Schedule maintenance](#schedule-maintenance). To keep
copies off the host, see [off-host copies](remote-backups.md) or download archives to
another machine.

### Restore a backup

To restore, stop the server, select an archive, and confirm the world name. Restore
replaces the server's entire `worlds_local` directory and leaves the server stopped.
Start it after checking the job result.

An archive holds the server's whole `worlds/` directory except a top-level `cache/`
directory and the game's own rolling saves (`<world>_backup_auto-*` in `worlds_local`):
`worlds_local` with every world in it, and the player lists `adminlist.txt`,
`bannedlist.txt`, and `permittedlist.txt` beside it. A restore unpacks only the
`worlds_local` part and swaps it in by rename, so it replaces every world in that
directory, not only the one the backup was named for. The game's rolling saves already on
disk are moved into the restored directory, so a restore does not delete them. It does not
touch the player lists, mods, configuration, or server files, so the player lists in an
archive are never restored.

A restore needs a stopped server and is refused with `409 instance_must_be_stopped`
otherwise. It first takes a **pre restore** archive of the current `worlds/` directory,
with the same contents as any other archive and kept apart from your own backups, so the
world you replace can be restored in turn. It
cannot be cancelled. If it fails, the server is set to the `error` state and its
overview shows **This server needs a check** until you choose **Check this server**.

## Recover a saved setup

On **Saved setups**, stop the server and save a named setup before changing its mods
or settings. Valmin stores the installed package files along with mod versions,
registries, locks, enabled states, client tags, launch and backup settings, and
flat `.cfg` files supported by the configuration editor. The saved package files
support recovery when a registry or download cache is unavailable. If a required
package cannot be captured, the save job fails without publishing the setup.

You can link a consistent backup from the same server. Linked backups do not count
toward retention and cannot be deleted until all setups that link to them are
deleted. A linked backup is a reference to an existing world archive, not a second
copy of it.

Before restoring, compare the setup with the current server and review the preview.
Stop the server, then start the restore. The preview expires when the server state
changes; reload it before retrying. Valmin verifies the saved packages before
changing files and rolls back the setup if installation fails. The server remains
stopped. Check the job result before starting it.

The restore does not change the game build, selected world, server password, or
world files. To recover a world, restore the linked archive separately on **Backups**.
World restoration follows the [world restore procedure](#back-up-or-restore-a-world).

## Schedule maintenance

On **Maintenance**, choose **What to run** and **How often**, then choose
**Add schedule** to run backups, restarts, or game updates on their own.

New schedules use the browser's time zone, including across daylight saving changes.
Existing schedules keep the time zone they were created with (UTC for schedules created
before this change). Upcoming runs, each schedule's next run, and skipped and failed
runs are shown in your browser's time zone. Each schedule names the zone its cron
expression uses.
**Upcoming runs** lists the next runs of every enabled schedule, earliest first.

A scheduled restart or backup stops the server and disconnects its players. To avoid that,
turn on **Wait until no players are connected** when you add the schedule. A due run then
waits until the server is empty. **Wait at most** caps the wait; after it, the run starts
with players connected. Valmin reads the player count from the server log, and it can be
unknown for up to 10 minutes after the server starts. **If the player count is unknown**
chooses what that means: **Wait, as if players are connected** or **Run, as if the server
is empty**. A waiting run shows a **waiting for players** badge in the schedule list, heads
**Upcoming runs**, and shows a notice on the server's overview page, each with the latest
time it will start.

If the server has the `Tristan-ValheimRcon` mod, Valmin warns players in chat with the
mod's `say` command when a run starts waiting, and again 5 minutes before the latest
time. A wait of 5 minutes or less gets only the first warning. Without the mod, the run
waits the same way but players are not told.

**Skipped and failed runs** lists the scheduled runs, among the last 50, that did not
complete. A run is **Skipped** when the server is busy with another task or is in a state
the task cannot run from. A scheduled game update on a modded server is always skipped,
because the update needs a person to confirm it. A **Failed** run started and did not
finish. A **Cancelled** run was cancelled by a person, or stopped when Valmin shut down.
Skipped and failed runs show the reason under them.

## Stop a server when nobody plays

An idle server keeps using CPU with nobody connected. To save power, open **Maintenance**,
turn on **Stop when empty** under **Auto-stop**, choose how many minutes (5 to 1440) the
server may sit empty, and select **Save auto-stop**. Once the server has had no players for
that long, Valmin stops it the normal way, saving the world first. Players cannot join until
someone starts it again.

Valmin reads the player count from the server log. While the count is unknown, the server
counts as occupied, and the count can stay unknown for up to 10 minutes after the server
starts. A server nobody joins therefore stops at most about 10 minutes plus the chosen delay
after it starts. The idle time restarts from zero when Valmin itself restarts, and an
auto-stop waits while another task is running on the server.

An auto-stop appears in the job history as requested by **Panel**, in the audit log as
**Auto-stop**, and sends `server_stopped` with the reason **No players for N minutes** to the
destinations of a **Server stopped** [alert rule](usage.md#get-notified) covering the server.

## Reset or upgrade the world with mods

**World tools** on **Maintenance** runs two world-maintenance mods from the panel:
[Upgrade World](https://thunderstore.io/c/valheim/p/JereKuusela/Upgrade_World/) and
[FreshWorld](https://thunderstore.io/c/valheim/p/sighsorry/FreshWorld/). Both run inside the
game, which is the only safe way to change a world's zones. Install the mod you want and
`Tristan-ValheimRcon` from **Mods**; until then the actions are greyed out with a link to the mod.
The card is shown to administrators only, because the commands go over RCON unrestricted.

| Action          | Mod           | Sends                         | Does                                                                                   |
| --------------- | ------------- | ----------------------------- | -------------------------------------------------------------------------------------- |
| Reset zones     | Upgrade World | `zones_reset start [min=<m>]` | Empties generated zones away from player bases; they generate again when visited.     |
| Content upgrade | Upgrade World | `upgrade <operation> start`   | Adds content from a game update, such as `tarpits` or `bearcave`, to explored areas.  |
| World clean     | Upgrade World | `world_clean start`           | Removes objects and locations of uninstalled mods, duplicates and extra saved data.   |
| Run now         | FreshWorld    | `freshworld`                  | Restores zones, resources and locations as the mod's own config file describes.        |

The server must be stopped. After you type the world's name to confirm, Valmin backs up the
world as a **pre update** backup, starts the server, and sends the command over RCON. The
server keeps running. The job ends once the command is sent; the mod then works for a while
and reports its progress in the server log. The game saves the result at its next autosave or
when the server stops. If the result is not what you wanted, restore the pre update backup.

Upgrade World operations ending in `_worldgen` are refused, because they move rivers and can
destroy bases. Upgrade World accepts commands from the server console only while its
**Root users** setting is empty or contains `-1`. Base protection follows each mod's own
config file.

## Stop every server before a power cut

If the host runs on an outage timetable, open **Power cuts** in the header's
**Administration** menu, enter the date and time the power goes off, and select
**Add power cut**. The time is read in your [time zone](usage.md#set-your-time-zone-and-time-format).
Seven minutes before the cut, players on a server with the RCON mod are told in chat that
it shuts down in five minutes; a player who joins later is still told. Two minutes before
the cut, Valmin stops every running server the normal way, saving the world first, and
keeps stopping any server that is started again until the power goes off. A server busy
with another task, such as a backup, is stopped as soon as the task ends. From 15 minutes
before a cut, scheduled backups, restarts and game updates are skipped, and queued mod
installs wait until after it, so none of them is cut off midway.

At the same moment, Valmin sends one `power_cut_soon` notification with the time of the
cut, when the servers stop, and which servers are running, to the destinations of every
**Power cut soon** [alert rule](usage.md#get-notified). The stops
appear in the audit log as **Planned shutdown**. Stopped servers stay stopped
when the host comes back; start them from the panel or with the Discord bot's `/start`.
Passed power cuts disappear from the list. Bot admins can plan and cancel power cuts from
Discord too; see [Use the Discord bot](usage.md#use-the-discord-bot).

The panel itself survives a power cut: its database can lose the last few seconds of
changes but is never left damaged, and on the next start it marks interrupted jobs as
failed and matches every server's state to Docker.

## Back up the whole installation

A world archive does not include panel accounts, settings, secrets, or installed
mods. With the default configuration, the data root contains:

| Path                     | Contents                                                         |
| ------------------------ | ---------------------------------------------------------------- |
| `panel.db`               | SQLite database, with WAL sidecar files while in use.            |
| `secret.key`             | Master key for stored secrets. Keep it with the database backup. |
| `instances/<id>/server/` | Game installation, mods, and their configuration.                |
| `instances/<id>/worlds/` | Saves and player list files.                                     |
| `instances/<id>/logs/`   | Game logs.                                                       |
| `backups/`               | World backup archives and pre-upgrade database copies.           |
| `cache/steam/896660/`    | Downloaded game builds.                                          |
| `cache/<registry>/`      | Downloaded mod packages, one directory per registry.             |
| `setups/blobs/`          | Package files retained for saved setups.                         |

For a full offline copy, stop every game server through the panel and wait for
active jobs to finish. From `deploy/`, run `docker compose stop valmind`, then copy
the entire host data root to separate storage, preserving ownership and permissions.
Save `deploy/.env`, `deploy/compose.override.yaml` if used, and any Compose or Caddy
changes too. Caddy's certificates and local CA live separately in the `caddy_data`
Docker volume. Preserve that volume to keep existing client certificate trust;
it is not included in a copy of `/srv/valmin`.
After the copy finishes, run `docker compose start valmind` and start your game
servers through the panel.

Game containers run alongside Compose services. Stopping the Compose stack does
not stop those game containers. On recovery, restore the data with UID/GID
`10000:10000`, start the panel, and start the servers you want running.

## Update the panel or game

Before a panel upgrade, [take a full backup](#back-up-the-whole-installation). From the repository root at the revision
you want to deploy, rebuild and recreate the panel:

```sh
make panel-image
cd deploy
docker compose up -d --force-recreate valmind
docker compose logs --tail=100 valmind
```

After the panel restarts, open **Diagnostics** to confirm it reaches Docker, the data
root, and the images it needs. See [troubleshooting](troubleshooting.md#check-the-diagnostics-page).

For registry deployments, update `VALMIN_IMAGE` to the intended digest and pull it
before recreating the service. Migrations run at startup and are forward-only.

Before it applies a migration, the panel copies the database to
`backups/panel-pre-<version>-<time>.db` under the data root, where `<version>` is the
first migration about to run. The copy is readable only by the panel account. The
three newest copies are kept; older ones are deleted. If the copy fails, the panel
does not start. The startup log names the file.

An older build refuses to start on a database that a newer build has migrated. To
roll back:

1. From `deploy/`, run `docker compose stop valmind`.
2. In the data root, replace `panel.db` with the matching `backups/panel-pre-*.db`
   copy, and delete `panel.db-wal` and `panel.db-shm` if they exist. Keep the owner
   `10000:10000`.
3. Set the previous image and run `docker compose up -d valmind`.

Changes made in the panel after the upgrade are lost. World files are not part of
the database and are not affected.

Use the server's update action to update the Valheim installation. Rebuilding the
runtime image alone does not download a new game build. Preload any new runtime
or SteamCMD image before configuring Valmin to use it: the daemon does not pull images.

## Rotate encryption keys

Server passwords, RCON passwords, webhook URLs and the Discord bot token are encrypted
with keys derived from the master key in `secret.key`. Administrators open **Encryption
keys** in the header's **Administration** menu and choose **Rotate derived keys** to
re-encrypt every stored secret under a new key generation. Nothing has to be entered
again and no server is stopped. If a rotation is interrupted, running it again finishes
what is left.

Rotation does not replace the master key. If `secret.key` itself has leaked, rotating
does not protect the secrets: restore onto a host with a new master key and enter the
passwords again.

## Recover from a lost master key

`secret.key` encrypts server passwords, RCON passwords, webhook URLs, and the
Discord bot token in `panel.db`. The panel creates it only on a first start. Later, if the file is
missing or does not match the database, the panel refuses to start.

First, restore `secret.key` from the backup taken with this `panel.db`, with owner
`10000:10000` and mode `0600`, and start the panel.

If the key is lost for good, accept a new one from `deploy/`:

```sh
docker compose run --rm --no-deps valmind valmind admin accept-new-key --confirm-key-loss
```

The command creates a new `secret.key` when the file is missing, or uses the key
set in `VALMIN_MASTER_KEY` or `VALMIN_MASTER_KEY_FILE`. Then it:

- gives each server whose password it cannot read a new random password, and prints
  each server's name and password. The next start recreates the container;
- clears unreadable RCON passwords. They are read again from the plugin config;
- disables webhook destinations whose URL it cannot read, and lists them. Send each
  URL again with `PATCH /api/v1/admin/webhooks/{id}` and `{"url": "...", "enabled": true}`,
  or delete the destination on the **Notifications** page and add it again, then add it
  back to its alert rules;
- turns off the Discord bot and clears its token if it cannot read it. Paste the token
  again on the **Discord bot** page and turn the bot back on;
- keeps sessions valid. Reload open panel tabs to get a new CSRF token.

If the key already matches, it reports that and changes nothing. Running it again
is safe. Back up the new `secret.key` with the database.

## Change your own password

Any signed-in account, the owner included, can change its own password from the account
menu, without shell access. See [change your password](usage.md#change-your-password).
The account's other sessions end and the current one stays signed in. If you cannot
sign in, use [password recovery](troubleshooting.md#reset-an-account-password) instead.

The audit log records each change as `users.password.change` with the detail
`{"target_user_id": "<your user id>"}` and the outcome `succeeded`. Neither password
appears in the entry, and an attempt the panel refuses, such as a wrong current password,
writes no entry.

## Restart after a reboot

The panel, Caddy, Docker proxy, and managed game containers use `unless-stopped`.
Docker restarts them after a reboot if they were left running. Deliberately stopped
containers stay stopped. When a game server comes back this way, Valmin sends a
"Server stopped unexpectedly" notification once it is running again. See [Docker restart policies](https://docs.docker.com/engine/containers/start-containers-automatically/).

Docker itself must start at boot. On a systemd host:

```sh
sudo systemctl enable docker
```

Before a planned reboot, stop game servers through the panel and let shutdown
finish so the game can save. Start those servers through the panel after reboot;
`unless-stopped` will preserve your deliberate stop.

From `deploy/`, check the stack after boot with `docker compose ps`. Check game
states in the panel; game containers are not listed as Compose services.

`docker compose down` removes the panel stack's containers and networks. Run
`docker compose up -d` to recreate them. Avoid `docker compose down -v` for routine
maintenance: it also deletes the named Caddy volumes, including the local CA.
