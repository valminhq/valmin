# Valmin

Valmin is a web panel for running Valheim dedicated servers on a single Linux host.
Create servers, import worlds, install Thunderstore mods, edit configuration, and
manage backups from your browser.

Each server runs in its own Docker container. World saves stay in ordinary
host directories. The Go daemon, `valmind`, serves the Svelte frontend and stores
accounts, server settings, and job history in SQLite. The runtime image contains
no game files; SteamCMD downloads them into a shared build cache, which Valmin
copies into each server's writable installation.

- Start, stop, restart, clone, and update servers.
- Watch live logs, resource usage, and player activity.
- Install mods with dependency resolution and edit BepInEx configuration.
- Back up and restore worlds, set retention, and schedule maintenance.
- Invite users and grant access to individual servers.
- Publish optional server status pages and configure webhook notifications.
- Check the deployment's health and export a redacted support bundle.

The console currently displays logs only. Sending commands is not implemented.
SQLite is the only supported database.

## Get started

Use the [installation guide](docs/installation.md) to build the images and run
Valmin with Docker Compose. It covers host preparation, HTTPS, game ports, and
creating the first administrator account.

The supplied deployment requires Linux x86-64 and a local Docker Engine. It runs
the panel and game servers as UID/GID `10000:10000`. The default memory limit is
4096 MiB per game server.

## Documentation

| Task                                           | Guide                                                |
| ---------------------------------------------- | ---------------------------------------------------- |
| Deploy Valmin                                  | [Installation](docs/installation.md)                 |
| Create servers, import worlds, and manage mods | [Use the panel](docs/usage.md)                       |
| Set paths, images, ports, and daemon options   | [Configuration](docs/configuration.md)               |
| Protect your data or update Valmin             | [Backups, restore, and upgrades](docs/operations.md) |
| Diagnose a failure or reset a password         | [Troubleshooting](docs/troubleshooting.md)           |
| Integrate with the panel                       | [HTTP API and WebSockets](docs/api.md)               |
| Build, run, and test changes                   | [Development](docs/development.md)                   |

The [documentation index](docs/README.md) also links to the architecture overview
for contributors.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for bug reports, development checks, and pull requests.
