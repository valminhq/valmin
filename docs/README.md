# Valmin documentation

## Run a panel

Start with [installation](installation.md), then [create your first server](usage.md#create-a-server).
For home WiFi, use the [LAN setup](installation.md#local-wifi-or-lan) and
[certificate trust instructions](installation.md#trust-the-local-certificate).

| Guide                                           | Contents                                                                                       |
| ----------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| [Installation](installation.md)                 | Build images, prepare the host, configure HTTPS, open game ports, and create an administrator. |
| [Use the panel](usage.md)                       | Servers, worlds, mods, configuration, players, templates, access, alerts, and the Discord bot. |
| [Backups, restore, and upgrades](operations.md) | World backups, schedules, auto-stop, power cuts, full installation copies, keys, and updates.  |
| [Remote backups](remote-backups.md)             | Copy world backups to WebDAV or any rclone remote, and recover after losing the host.          |
| [Configuration](configuration.md)               | Daemon settings, environment variables, and host/container paths.                              |
| [Troubleshooting](troubleshooting.md)           | Diagnostics page, support bundle, logs, startup failures, and password recovery.               |

## Find a feature

| I want to                                       | Read                                                                                       |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------ |
| Create a server, with mods or an existing world | [Create a server](usage.md#create-a-server)                                                |
| Copy a server                                   | [Clone a server](usage.md#clone-a-server)                                                  |
| Bring back a server the panel lost track of     | [Recover an unclaimed server](usage.md#recover-an-unclaimed-server)                        |
| Rename a server or change its password          | [Change server settings](usage.md#change-server-settings)                                  |
| Set memory and CPU limits                       | [Resource limits](usage.md#resource-limits)                                                |
| Update Valheim                                  | [Update the game](usage.md#update-the-game)                                                |
| Delete a server                                 | [Delete a server](usage.md#delete-a-server)                                                |
| Read the console or send commands               | [Send server commands](usage.md#send-server-commands)                                      |
| Install, update, lock or downgrade mods         | [Manage mods](usage.md#manage-mods)                                                        |
| Give players the mod list they need             | [Tell players which mods they need](usage.md#tell-players-which-mods-they-need)            |
| Edit mod settings                               | [Edit mod configuration](usage.md#edit-mod-configuration)                                  |
| Import, reset or roll back a world              | [Manage worlds](usage.md#manage-worlds)                                                    |
| Back up and restore a world                     | [Back up or restore a world](operations.md#back-up-or-restore-a-world)                     |
| Keep backups off the host                       | [Remote backups](remote-backups.md)                                                        |
| Undo a bad mod or config change                 | [Save and restore a setup](usage.md#save-and-restore-a-setup)                              |
| Share a server's setup with another panel       | [Share a server as a template](usage.md#share-a-server-as-a-template)                      |
| See how two servers differ                      | [Compare two servers](usage.md#compare-two-servers)                                        |
| Ban, allow or make players admins               | [Player lists](usage.md#player-lists)                                                      |
| See when people play                            | [Player activity](usage.md#player-activity)                                                |
| Show server status to people without an account | [Public status page](usage.md#public-status-page)                                          |
| Give friends access                             | [Share access](usage.md#share-access)                                                      |
| Back up, restart or update on a timetable       | [Schedule maintenance](operations.md#schedule-maintenance)                                 |
| Stop servers nobody is playing on               | [Stop a server when nobody plays](operations.md#stop-a-server-when-nobody-plays)           |
| Reset or upgrade the world with mods            | [Reset or upgrade the world with mods](operations.md#reset-or-upgrade-the-world-with-mods) |
| Shut everything down before a power outage      | [Stop every server before a power cut](operations.md#stop-every-server-before-a-power-cut) |
| Get alerts in Discord or elsewhere              | [Get notified](usage.md#get-notified)                                                      |
| Let friends start servers from Discord          | [Use the Discord bot](usage.md#use-the-discord-bot)                                        |
| See who changed what                            | [Read the audit log](usage.md#read-the-audit-log)                                          |
| Check the panel's health or report a bug        | [Check the diagnostics page](troubleshooting.md#check-the-diagnostics-page)                |
| Back up the whole panel or upgrade it           | [Back up the whole installation](operations.md#back-up-the-whole-installation)             |
| Set my time zone or change my password          | [Your account](usage.md#your-account)                                                      |

## Integrate or contribute

- [Contributing](../CONTRIBUTING.md): issue reports, contribution workflow, and checks before a PR.
- [HTTP API and WebSockets](api.md): authentication, request examples, jobs, common endpoints, and subscriptions.
- [Development](development.md): local setup, stub servers, builds, and checks.
- [Frontend](../web/README.md): run and check the UI separately.

## Understand the implementation

[Architecture](architecture.md) explains the daemon, frontend, Docker deployment,
storage layout, and background jobs. Public guides describe implemented behavior;
source links point to the code that defines the contract.
