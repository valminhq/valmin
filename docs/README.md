# Valmin documentation

## Run a panel

Start with [installation](installation.md), then [create your first server](usage.md#create-a-server).

| Guide                                           | Contents                                                                                       |
| ----------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| [Installation](installation.md)                 | Build images, prepare the host, configure HTTPS, open game ports, and create an administrator. |
| [Use the panel](usage.md)                       | Create servers, import saves, manage mods, and share access.                                   |
| [Configuration](configuration.md)               | Daemon settings, environment variables, and host/container paths.                              |
| [Backups, restore, and upgrades](operations.md) | World backups, full installation copies, recovery, and updates.                                |
| [Troubleshooting](troubleshooting.md)           | Logs, startup failures, connection problems, and password recovery.                            |

## Integrate or contribute

- [HTTP API and WebSockets](api.md): authentication, request examples, jobs, common endpoints, and subscriptions.
- [Development](development.md): local setup, stub servers, builds, and checks.
- [Frontend](../web/README.md): run and check the UI separately.

## Understand the implementation

[Architecture](architecture.md) explains the daemon, frontend, Docker deployment,
storage layout, and background jobs. Public guides describe implemented behavior;
source links point to the code that defines the contract.
