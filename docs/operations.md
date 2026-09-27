# Back up, restore, and upgrade

[Documentation](README.md) / Back up, restore, and upgrade

## Back up or restore a world

On **Backups**, choose **Stop and back up** for a consistent world archive. Valmin
stops a running server, waits for shutdown, makes the archive, and starts it again.
A server that was already stopped stays stopped.

**Back up without stopping** is best-effort: an active save can produce an incomplete
archive. Download backups to another machine. Configure retention and schedules
on the same page; a retention count of `0` keeps all backups of that type.
Snapshots taken before a restore, import, or update have their own limit, equal to
the stopped-server count, so they never push out your backups.

Schedules run in UTC, and their run times are shown in UTC.

The history lists the most recent archives. Use **Load older backups** below it to
reach the rest of what retention has kept.

To restore, stop the server, select an archive, and confirm the world name. Restore
replaces the server's entire `worlds_local` directory and leaves the server stopped.
Start it after checking the job result.

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

## Recover from a lost master key

`secret.key` encrypts server passwords, RCON passwords, and webhook URLs in
`panel.db`. The panel creates it only on a first start. Later, if the file is
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
- keeps sessions valid. Reload open panel tabs to get a new CSRF token.

If the key already matches, it reports that and changes nothing. Running it again
is safe. Back up the new `secret.key` with the database.

## Restart after a reboot

The panel, Caddy, Docker proxy, and managed game containers use `unless-stopped`.
Docker restarts them after a reboot if they were left running. Deliberately stopped
containers stay stopped. See [Docker restart policies](https://docs.docker.com/engine/containers/start-containers-automatically/).

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
