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

## Source map

| Path                                    | Responsibility                                                        |
| --------------------------------------- | --------------------------------------------------------------------- |
| `cmd/valmind`                           | Startup, shutdown, healthcheck, diagnose, and recovery commands.      |
| `internal/api`                          | HTTP handlers and request middleware.                                 |
| `internal/auth`, `internal/authz`       | Authentication and authorization.                                     |
| `internal/store`                        | SQLite queries and migrations.                                        |
| `internal/jobs`                         | Background job execution, locks, and progress.                        |
| `internal/instance`, `internal/runtime` | Server lifecycle and Docker operations.                               |
| `internal/backup`                       | World archives, verification, and restore.                            |
| `internal/diag`                         | Health checks and the redacted support bundle.                        |
| `internal/mods`                         | Thunderstore packages, dependencies, installation, and configuration. |
| `internal/scheduler`, `internal/notify` | Scheduled work and webhook delivery.                                  |
| `internal/ws`                           | WebSocket subscriptions and event delivery.                           |
| `web`                                   | Frontend and embedded assets.                                         |
| `docker`, `deploy`                      | Container images and Compose deployment.                              |
