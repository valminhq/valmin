# Architecture

[Documentation](README.md) / Architecture

Valmin manages Valheim servers on one Linux host. One daemon owns the database,
background jobs, and access to that host's Docker Engine.

## Deployment

The supplied Compose deployment contains three services:

| Service             | Responsibility                                                            |
| ------------------- | ------------------------------------------------------------------------- |
| Caddy               | Terminates HTTPS and forwards browser requests and WebSocket connections. |
| `valmind`           | Serves the UI and API, checks permissions, and manages servers.           |
| Docker socket proxy | Gives the daemon access to the Docker API used for container management.  |

Game containers are created by `valmind` on the same Docker host. They are not
Compose services, so `docker compose stop` does not stop them. Players connect to
the game containers through published UDP ports; their traffic does not pass
through Caddy.

Three networks keep the pieces apart:

| Network        | Subnet           | Members                     | Purpose                                                    |
| -------------- | ---------------- | --------------------------- | ---------------------------------------------------------- |
| `docker`       | `10.89.14.0/24`  | Daemon, socket proxy        | Docker API access. Internal, with no route off the host.   |
| `valmin`       | `10.89.13.0/24`  | Caddy, daemon               | Browser traffic from Caddy to the daemon.                  |
| `valmin-games` | `10.89.15.0/24`  | Daemon, every game container | Console commands from the daemon to the game's RCON port. |

Every service has a fixed address, so the trusted proxy address is known in advance.
The daemon takes the last address on the game network, because Docker gives
restarted game containers the lowest free addresses. Only Caddy publishes the panel's
HTTP and HTTPS ports. Although the proxy restricts the Docker API, the remaining
container creation access is still root-equivalent on the host.

The deployment definition is [deploy/compose.yaml](../deploy/compose.yaml).

## Game installation and storage

The game image provides the runtime environment, without distributing the game
files. Valmin starts a temporary SteamCMD container to download a dedicated server
build into `cache/steam/896660/<build-id>/`. Each instance gets its own writable
copy under `instances/<id>/server/`.

Mod packages are downloaded once and cached under `cache/<registry>/`, shared by
every server that installs them. Each registry has its own directory: two registries
can publish different files under one package name and version, so a shared directory
could hand one registry's download to an install of the other's. Every installed
package has a file manifest, which is what makes uninstall, disable and rollback
exact.

A game container mounts its installation, worlds, and logs. The backup archive
directory is not mounted into it. All managed processes use UID/GID `10000:10000`
so the daemon and containers can work with the same files.

The daemon can see a different absolute path from Docker's host. For example,
`/mnt/games/valmin` on the host can be mounted at `/srv/valmin` in the panel. At
startup, a temporary container verifies that both paths reach the same data.
See [configuration](configuration.md) for these settings.

SQLite stores accounts, permissions, server definitions, jobs, schedules, and backup
metadata. The `secret.key` file supplies the master key for encrypted secrets. A world
archive is not a backup of those files; see
[full installation backups](operations.md#back-up-the-whole-installation).

## Jobs and recovery

Long operations run as jobs. The HTTP handler records the job and returns its ID;
the browser follows progress through HTTP and WebSocket messages. A per-instance
lock prevents conflicting operations from changing the same server at once. A
transaction covers the change of state, never the work itself, so a slow Docker call
or file copy never holds the database.

The daemon holds a database lease to prevent multiple active daemons from managing
the same installation. On startup, the supervisor removes helper containers a killed
daemon left behind, closes out the jobs a crash interrupted, reconciles database records with
Docker containers, and resumes starts that a crash cut short. Interrupted multi-step
operations, such as provisioning, wait for an operator to resume or abandon them.

After startup, the supervisor runs the same reconciliation every 10 seconds. It joins
containers to servers by their labels rather than by stored container IDs, so it also
finds containers the database has lost; those appear as unclaimed servers that an
administrator can recover. The same pass notices a container that Docker restarted on
its own, for example after a host reboot, and raises an unexpected-stop alert. It also
runs the auto-stop check, which stops a server that has had no players for its
configured time. An unknown player count counts as occupied.

Stops use `SIGINT` with at least 120 seconds for graceful shutdown. A stop is recorded
as clean only when the log shows the world save finishing. A consistent world backup
waits for the server to stop before copying its save files. A hot backup copies while
the game may still write, so it is best-effort.

## Logs and console commands

For each running container, the daemon follows the Docker log stream with timestamps
into an in-memory ring buffer, bounded by both line count and size. The startup
segment is kept separately so the browser can always show how the server started.
Anchored patterns over that stream detect readiness, world saves, player counts,
joins and leaves, crossplay join codes and the mod loader's results. Operators can
[override a pattern](configuration.md#override-log-patterns) if a game update changes a
line. Player observations are recorded for the activity history.

The game does not read its standard input, so console commands go through the
`Tristan-ValheimRcon` mod. When the mod is installed, the daemon writes its
configuration with a random password, stores the password encrypted, and opens a new
authenticated RCON connection over the game network for each command. The RCON port
is never published on the host.

## Mods and configuration

Mod changes run as jobs that resolve the full dependency plan first, back up the world
when one exists, and roll back every package if any step fails. Installs and updates
requested while a server runs are queued; once the server stops, the queue runs, with
queued updates combined into one job and one world backup. A restart requested while
installs are queued stops the server, runs them, and starts it again.

Configuration files are parsed into a document that keeps comments and layout, so a
form edit changes only the values it touches. The previous bytes are kept beside the
file for comparison. A file edited while its server runs is also kept as a pending
copy: many plugins write their loaded settings back at shutdown, so the daemon
reapplies the pending values once the server has stopped and before it starts again.

## Scheduler and power cuts

The scheduler checks once a minute for due schedules and submits each as a normal job,
or records why it skipped the run. It never runs work itself. A run that waits for
players to leave is held and retried until the server is empty or its maximum wait
passes. A schedule missed while the daemon was down fires once, not once per missed
time.

A separate clock checks planned power cuts every 10 seconds. It warns players before
the cut, stops every running server shortly before it, and keeps stopping servers that
start again until the cut has passed. In the window before a cut, scheduled runs are
skipped and queued mod installs wait until it has passed.

## Notifications

Conditions such as a crash loop, a failed job or low disk space are evaluated by a
periodic scan that compares the current state with the open conditions and records
each change. One-off events, such as a server starting or stopping, are recorded when
they happen. Both go through alert rules to their destinations as durable webhook
deliveries, which are retried and survive a daemon restart.

## Browser and API

The SvelteKit frontend builds as a static SPA embedded in the Go binary. Production
needs no separate Node.js service. During development, Vite serves the frontend
and proxies `/api` traffic to the daemon.

HTTP requests use session cookies and CSRF protection. Every handler checks the
caller's permission for its action on that server; authorization is not done by route
pattern. Members see only servers for which they have a grant, and a server they cannot
see answers `404`, not `403`. The same checks apply per WebSocket topic. The
[API guide](api.md) documents login, jobs, and subscriptions.

A password change ends the account's other sessions in the same transaction that
stores the new hash, and their WebSockets close after it commits. An administrator's
reset ends every session of the account.

## Source map

| Path                                             | Provides                                                                                                                         |
| ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/valmind`                                    | Startup gate, shutdown, healthcheck, diagnose and key-recovery commands.                                                         |
| `internal/config`                                | Settings precedence, validation and the host data path check.                                                                    |
| `internal/version`                               | The build version reported by the daemon and the release artefacts.                                                              |
| `internal/api`                                   | HTTP handlers and server assembly: authorize, load, pre-check, call the owning package.                                          |
| `internal/api/errors`, `internal/api/middleware` | The error envelope and the request middleware chain.                                                                             |
| `internal/errcode`                               | The closed registry of error codes shared by responses and job rows.                                                             |
| `internal/ratelimit`                             | A keyed token-bucket limiter.                                                                                                    |
| `internal/auth`, `internal/authz`                | Sessions, passwords and invites; the single `Can` check.                                                                         |
| `internal/store`, `internal/store/storetest`     | SQLite queries and migrations; a migrated test database.                                                                         |
| `internal/jobs`                                  | The job engine: claims, locks, leases, progress and recovery of dead jobs.                                                       |
| `internal/runtime`                               | The Docker client and its fake.                                                                                                  |
| `internal/instance`                              | The instance state machine and its validated writes, paths, ports, log reader and container spec.                                |
| `internal/instance/control`                      | Every job that changes an instance: submission, claims, runners, crash recovery, the mod queue, auto-stop and the supervisor.    |
| `internal/instance/history`                      | Recording of player observations and identities.                                                                                 |
| `internal/command`                               | The RCON command channel: discovery, configuration and sending.                                                                  |
| `internal/backup`                                | World archives and restore staging.                                                                                              |
| `internal/backup/remote`                         | Off-host storage backends (rclone, WebDAV).                                                                                      |
| `internal/backup/remotecopy`                     | The jobs that copy archives off-host and apply remote retention.                                                                 |
| `internal/crypto`                                | The master key, derived keys and the encryption envelope.                                                                        |
| `internal/crypto/rotation`                       | The key-rotation job.                                                                                                            |
| `internal/diag`                                  | The diagnostics report and support bundle.                                                                                       |
| `internal/diag/deep`                             | The diagnostic checks that need a container, as a job.                                                                           |
| `internal/alerts`                                | Condition evaluation and alert-rule matching, as pure functions.                                                                 |
| `internal/alerts/scan`                           | The alert scan job: gather, evaluate, reconcile, dispatch.                                                                       |
| `internal/mods/*`                                | Registry clients and caches, archive extraction, the dependency resolver, file placement, shared file helpers and the `.cfg` parser. |
| `internal/mods/manager`                          | Mod install, update, toggle and uninstall jobs with rollback and recovery, and the registry sync.                                |
| `internal/setupblob`                             | Retained package archives for saved setups.                                                                                      |
| `internal/sharecode`                             | Template code format: encode and decode.                                                                                         |
| `internal/scheduler`                             | The schedule clock, the submitter that turns a due schedule into a job or a skip, and the planned power cut clock.               |
| `internal/notify`                                | Notification events, rendering and the outbound sender.                                                                          |
| `internal/notify/delivery`                       | Durable webhook deliveries for domain events and alert edges, and the jobs that send them.                                       |
| `internal/discord`                               | The Discord bot: the Gateway connection and the `/status`, `/start` and `/shutdown` commands.                                    |
| `internal/ws`                                    | WebSocket subscriptions and event delivery.                                                                                      |
| `web`                                            | Frontend and embedded assets.                                                                                                    |
| `docker`, `deploy`                               | Container images and Compose deployment.                                                                                         |

## Off-host backup copies

The archive catalog transaction also inserts remote upload intent when both the
destination and instance policy are enabled. A dispatcher submits remote copy
jobs through the existing engine, using a dedicated remote lock. Remote jobs do
not hold the instance lifecycle lock or delay the restart owed by a cold backup.

The remote-copy catalog survives local pruning and job-history expiration.
Adapter operations are Put, Stat, and Delete; retry policy, retention, and status
belong to the service. WebDAV uses checked outbound addresses; rclone runs fixed
commands against operator-owned configuration.

Temporary source protection is rechecked in deletion transactions. Files selected
for local pruning are unlinked only after confirming their catalog row is gone.
See [remote backup operations](remote-backups.md) for recovery and storage policy.

## Discord bot

The bot runs inside the daemon as one outbound WebSocket connection to Discord's
Gateway, so nothing new listens on the host. It identifies with no intents,
answers Discord's heartbeat about every 41 seconds, and reconnects with backoff.
A rejected token stops it until its settings change. Commands are registered
only in Discord servers that some link names.

Each interaction is resolved to a link: the channel's own link, then its parent
channel's for a thread, then the whole Discord server's. The link lists the
servers the channel may see and whether it may start them. `/start` goes through
the same start job and claim as the panel's Start button, with no panel user and
an audit entry naming the Discord user. `/shutdown` is limited to the configured bot
admins and writes to the same planned power cut table as the panel. The token is
stored encrypted like other secrets and rotates with them.
