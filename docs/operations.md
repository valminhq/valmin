# Back up, restore, and upgrade

[Documentation](README.md) / Back up, restore, and upgrade

## Back up or restore a world

On **Backups**, choose **Stop and back up** for a consistent world archive. Valmin
stops a running server, waits for shutdown, makes the archive, and starts it again.
A server that was already stopped stays stopped.

**Back up without stopping** is best-effort: an active save can produce an incomplete
archive. Download backups to another machine. Configure retention and schedules
on the same page; a retention count of `0` keeps all backups of that type.

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
| `backups/`               | World backup archives.                                           |
| `cache/steam/896660/`    | Downloaded game builds.                                          |

For a full offline copy, stop every game server through the panel and wait for
active jobs to finish. From `deploy/`, run `docker compose stop valmind`, then copy
the entire host data root to separate storage, preserving ownership and permissions.
Save `deploy/.env` and any Compose or Caddy changes too.
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

For registry deployments, update `VALMIN_IMAGE` to the intended digest and pull it
before recreating the service. Migrations run at startup and are forward-only;
a rollback may require restoring the pre-upgrade data backup.

Use the server's update action to update the Valheim installation. Rebuilding the
runtime image alone does not download a new game build. Preload any new runtime
or SteamCMD image before configuring Valmin to use it: the daemon does not pull images.
