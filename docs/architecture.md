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

The socket proxy and daemon share a private network. Caddy reaches the daemon on a
separate network. Only Caddy publishes the panel's HTTP/HTTPS ports. Although the
proxy restricts the Docker API, the remaining container creation access is still
root-equivalent on the host.

The deployment definition is [deploy/compose.yaml](../deploy/compose.yaml).

## Game installation and storage

The game image provides the runtime environment, without distributing the game
files. Valmin starts a temporary SteamCMD container to download a dedicated server
build into `cache/steam/896660/<build-id>/`. Each instance gets its own writable
copy under `instances/<id>/server/`.

Mod packages are downloaded once and cached under `cache/<registry>/`, shared by
every server that installs them. Each registry has its own directory: two registries
can publish different files under one package name and version, so a shared directory
could hand one registry's download to an install of the other's.

A game container mounts its installation, worlds, and logs. The backup archive
directory is not mounted into it. All managed processes use UID/GID `10000:10000`
so the daemon and containers can work with the same files.

The daemon can see a different absolute path from Docker's host. For example,
`/mnt/games/valmin` on the host can be mounted at `/srv/valmin` in the panel. At
startup, a temporary container verifies that both paths reach the same data.
See [configuration](configuration.md) for these settings.

SQLite stores accounts, permissions, server definitions, jobs, and backup metadata.
The `secret.key` file supplies the master key for encrypted secrets. A world archive
is not a backup of those files; see [full installation backups](operations.md#back-up-the-whole-installation).

## Jobs and recovery

Long operations run as jobs. The HTTP handler records the job and returns its ID;
the browser follows progress through HTTP and WebSocket messages. A per-instance
lock prevents conflicting operations from changing the same server at once.

The daemon holds a database lease to prevent multiple active daemons from managing
the same installation. On startup, it reconciles database records with Docker
containers and checks interrupted jobs and operations. Some failures need an
operator decision before the server can resume.

Stops use `SIGINT` with at least 120 seconds for graceful shutdown. A consistent
world backup waits for the server to stop before copying its save files. A hot
backup copies while the game may still write, so it is best-effort.

## Browser and API

The SvelteKit frontend builds as a static SPA embedded in the Go binary. Production
needs no separate Node.js service. During development, Vite serves the frontend
and proxies `/api` traffic to the daemon.

HTTP requests use session cookies and CSRF protection. Permissions are checked per
operation and per WebSocket topic. Members see only servers for which they have a
grant. The [API guide](api.md) documents login, jobs, and subscriptions.

A password change ends sessions in the same transaction that stores the new hash and its
audit record, and only after it commits does the daemon close the WebSockets those
sessions held. When an account changes its own password, every session except the
requesting one is removed, so that session, its cookie, its CSRF token, and its sockets
stay valid. The store's `SetUserPasswordKeepingSession` does this and returns the ids of
the sessions it revoked. `SetUserPasswordAudited`, used for an administrator's reset,
delegates to it with no session to keep, so a reset still ends every session of the
account, the target's own included.

## Source map

| Path | Provides |
| --- | --- |
| `cmd/valmind` | Startup gate, shutdown, healthcheck, diagnose and key-recovery commands. |
| `internal/api` | HTTP handlers and server assembly: authorize, load, pre-check, call the owning package. |
| `internal/api/errors`, `internal/api/middleware` | The error envelope and the request middleware chain. |
| `internal/errcode` | The closed registry of error codes shared by responses and job rows. |
| `internal/ratelimit` | A keyed token-bucket limiter. |
| `internal/auth`, `internal/authz` | Sessions, passwords and invites; the single `Can` check. |
| `internal/store`, `internal/store/storetest` | SQLite queries and migrations; a migrated test database. |
| `internal/jobs` | The job engine: claims, locks, leases, progress and recovery of dead jobs. |
| `internal/runtime` | The Docker client and its fake. |
| `internal/instance` | The instance state machine and its validated writes, paths, ports, log reader and container spec. |
| `internal/instance/control` | Every job that changes an instance: submission, claims, runners, crash recovery and the supervisor, built once by `control.New`. |
| `internal/instance/history` | Recording of player observations and identities. |
| `internal/backup` | World archives and restore staging. |
| `internal/backup/remote` | Off-host storage backends (rclone, WebDAV). |
| `internal/backup/remotecopy` | The jobs that copy archives off-host and apply remote retention. |
| `internal/crypto` | The master key, derived keys and the encryption envelope. |
| `internal/crypto/rotation` | The key-rotation job. |
| `internal/diag` | The diagnostics report and support bundle. |
| `internal/diag/deep` | The diagnostic checks that need a container, as a job. |
| `internal/alerts` | Condition evaluation and alert-rule matching, as pure functions. |
| `internal/alerts/scan` | The alert scan job: gather, evaluate, reconcile, dispatch. |
| `internal/mods/*` | Registry clients and caches, archive extraction, the dependency resolver, file placement and the `.cfg` parser. |
| `internal/mods/manager` | Mod install, update, toggle and uninstall jobs with rollback and recovery, and the registry sync. |
| `internal/setupblob` | Retained package archives for saved setups. |
| `internal/sharecode` | Template code format: encode and decode. |
| `internal/scheduler` | The schedule clock and the submitter that turns a due schedule into a job or a skip. |
| `internal/notify` | Notification events, rendering and the outbound sender. |
| `internal/notify/delivery` | Durable webhook deliveries for domain events and alert edges, and the jobs that send them. |
| `internal/ws` | WebSocket subscriptions and event delivery. |
| `web` | Frontend and embedded assets. |
| `docker`, `deploy` | Container images and Compose deployment. |

## Off-host backup copies

The archive catalog transaction also inserts remote upload intent when both the
destination and instance policy are enabled. A dispatcher submits remote_copy
jobs through the existing engine, using a dedicated remote lock. Remote jobs do
not hold the instance lifecycle lock or delay the restart owed by a cold backup.

The remote-copy catalog survives local pruning and job-history expiration.
Adapter operations are Put, Stat, and Delete; retry policy, retention, and status
belong to the service. WebDAV uses checked outbound addresses; rclone runs fixed
commands against operator-owned configuration. A native Drive adapter can use
opaque object identifiers without changing the queue.

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
an audit entry naming the Discord user. The token is stored encrypted like other
secrets and rotates with them.
