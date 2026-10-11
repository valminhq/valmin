# Valmin

Valmin is a self-hosted web panel for running several Valheim dedicated servers,
modded or not, on one Linux host. You create servers, install mods from Thunderstore
and Hexium, edit their configuration, and manage worlds and backups from the browser,
and you can give friends access to the servers you choose.

Each server runs in its own Docker container, and world saves stay in ordinary
directories on the host. The Go daemon, `valmind`, serves the web interface and
keeps accounts, server settings and job history in SQLite. The game image contains
no game files: SteamCMD downloads each game build once into a shared cache, and
every server gets its own copy.

## Features

**Servers**

- Create a server with a world preset, modifiers, crossplay, public listing, a memory
  limit and mods installed before the first start.
- Start, stop, restart, rename, clone and delete servers. Stops wait for the world save
  to finish.
- Update the game build when Steam publishes one, with a world backup first and every
  mod put back afterwards.
- Watch the live console with search, CPU, memory, disk use and the player count, and
  send console commands through the `Tristan-ValheimRcon` mod.
- Recover a server whose container survived but whose panel record was lost.

**Mods and configuration**

- Search Thunderstore and Hexium, install with dependency resolution, and review the
  full plan before anything changes.
- Update one mod or all of them in one job with rollback, pick any version, lock
  versions, disable a mod to find a broken one, and see which mods failed to load.
- Queue installs and updates on a running server; they run when it stops or restarts.
- Edit BepInEx and plugin configuration in a form built from the file, or as raw text,
  with a review step and comparison against earlier versions.
- Export the mods players need as a profile for r2modman, Gale or Thunderstore Mod
  Manager.

**Worlds and backups**

- Import a world from a single-player game or another server, restore one of the
  game's own rolling saves, or reset a world.
- Make consistent or best-effort backups, on demand, on every restart or on a
  schedule, with separate retention for each kind.
- Restore, download and delete backups, and copy them off the host over WebDAV or
  rclone (Google Drive, S3, OneDrive and others).
- Save a named setup of mods, configuration and settings, and roll back to it later.

**Sharing and people**

- Invite people with a single-use link and give each person viewer or operator access
  per server, with optional extra permissions.
- Share a server's setup as a template file or a short code that any Valmin panel can
  turn into a new server.
- Compare two servers side by side.
- Publish a public status page for a server, with a notice and joining instructions.
- Edit the admin, ban and allow lists, and see which accounts have reached a server.

**Automation and alerts**

- Schedule backups, restarts and game updates, optionally waiting until nobody is
  online and warning players in chat first.
- Stop a server automatically when nobody has played for a while.
- Plan power cuts: every server is stopped cleanly before the power goes off.
- Send alerts to Discord or any webhook for unexpected stops, crash loops, failed or
  stuck jobs, stale backups, low disk space, new game builds and more, with quiet hours.
- Let friends check and start servers from Discord with `/status` and `/start`.

**Running the panel**

- Player activity history, an audit log of every change with CSV export, and a
  diagnostics page with a redacted support bundle for bug reports.
- Secrets are encrypted at rest, and every action is checked against the user's
  permissions on that server.

## Get started

Use the [installation guide](docs/installation.md) to build the images and run
Valmin with Docker Compose. It covers host preparation, HTTPS, game ports, and
creating the first administrator account.

The supplied deployment requires Linux x86-64 and a local Docker Engine. It runs
the panel and game servers as UID/GID `10000:10000`. The default memory limit is
4096 MiB per game server. SQLite is the only supported database.

## Documentation

| Task                                          | Guide                                                |
| --------------------------------------------- | ---------------------------------------------------- |
| Deploy Valmin                                 | [Installation](docs/installation.md)                 |
| Create servers, manage mods, and share access | [Use the panel](docs/usage.md)                       |
| Back up worlds, schedule tasks, and upgrade   | [Backups, restore, and upgrades](docs/operations.md) |
| Copy backups to cloud storage                 | [Remote backups](docs/remote-backups.md)             |
| Set paths, images, ports, and daemon options  | [Configuration](docs/configuration.md)               |
| Diagnose a failure or reset a password        | [Troubleshooting](docs/troubleshooting.md)           |
| Integrate with the panel                      | [HTTP API and WebSockets](docs/api.md)               |
| Build, run, and test changes                  | [Development](docs/development.md)                   |

The [documentation index](docs/README.md) lists every feature and where it is
described.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for bug reports, development checks, and pull
requests. Inside the panel, **Report an issue** in the account menu opens a new issue.
