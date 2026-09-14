# Configuration reference

[Documentation](README.md) / Configuration reference

Compose reads `deploy/.env`, copied from [the example](../deploy/.env.example).
The daemon accepts flags, environment variables, and YAML in this
precedence order:

```text
flags > environment variables > YAML file > built-in defaults
```

The default YAML path is `/etc/valmin/config.yaml`. Override it with `--config` or
`VALMIN_CONFIG`. A key such as `server.external_url` becomes
`VALMIN_SERVER_EXTERNAL_URL` or `--server.external_url`.

Configuration is read at startup. Compose environment changes take effect when
the container is recreated; `docker compose restart` alone retains its old environment.

## Common settings

| Daemon environment variable                 | Purpose / default                                                                  |
| ------------------------------------------- | ---------------------------------------------------------------------------------- |
| `VALMIN_SERVER_EXTERNAL_URL`                | Required browser origin, including scheme and any nonstandard port.                |
| `VALMIN_SERVER_LISTEN`                      | HTTP listen address; `:8080`.                                                      |
| `VALMIN_SERVER_TRUSTED_PROXIES`             | Trusted proxy CIDRs; empty by default. Compose sets Caddy's address.               |
| `VALMIN_DATA_ROOT`                          | Data path visible to the daemon; `/srv/valmin`.                                    |
| `VALMIN_DATA_HOST_ROOT`                     | Required absolute path to that same directory on the Docker host.                  |
| `VALMIN_DOCKER_ENDPOINT`                    | Docker API endpoint; `unix:///var/run/docker.sock`. Compose uses the socket proxy. |
| `VALMIN_GAME_IMAGE`                         | Runtime image already present on the Docker host.                                  |
| `VALMIN_GAME_STEAMCMD_IMAGE`                | Download helper image; `steamcmd/steamcmd:latest`.                                 |
| `VALMIN_GAME_DEFAULT_MEM_MB`                | Default per-server memory limit; `4096`.                                           |
| `VALMIN_GAME_STOP_TIMEOUT`                  | Graceful stop timeout; `120s`, also the minimum.                                   |
| `VALMIN_PORTS_BASE` / `VALMIN_PORTS_STRIDE` | Port allocation start and spacing; `2456` / `5`.                                   |
| `VALMIN_LOG_LEVEL` / `VALMIN_LOG_FORMAT`    | Log verbosity and format; `info` / `json`.                                         |

## Compose and data paths

Adding an arbitrary variable to `deploy/.env` does not pass it into the container.
Add daemon overrides to `valmind.environment` in Compose too. Compose's
`VALMIN_STEAMCMD_IMAGE` maps to the daemon's `VALMIN_GAME_STEAMCMD_IMAGE`.

Valmin checks the data path mapping on startup. If `/mnt/games/valmin` is mounted
at `/srv/valmin`, the host root is `/mnt/games/valmin` and the daemon root is
`/srv/valmin`.

## Files, secrets, and value formats

Each daemon setting also accepts a `_FILE` environment variable, whose value is a
path to a file containing the setting. For example, `VALMIN_SERVER_EXTERNAL_URL_FILE`
reads the URL from a file visible inside the panel container. Setting both the
direct variable and its `_FILE` form is an error.

`VALMIN_SECRETS_MASTER_KEY_FILE` names the encryption key itself; it defaults to
`secret.key` under the data root. It is different from the generic `_FILE` mechanism.
The database defaults to `panel.db` under the same root. Preserve both in
[installation backups](operations.md#back-up-the-whole-installation).

Durations accept values such as `30s`, `24h`, and whole days such as `7d`.
Environment variables for lists, such as trusted proxy CIDRs, use comma-separated
values. Log levels are `debug`, `info`, `warn`, and `error`; formats are `json` and `text`.

See [the configuration source](../internal/config/config.go) for all settings and
defaults. Server settings, accounts, and schedules live in SQLite and are managed
through the panel.
