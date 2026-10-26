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

The daemon refuses to start on a YAML key it does not know, so a misspelt setting is an
error rather than silently ignored. It also refuses a stray argument, a job retention below
one day, a session lifetime of zero, and an absolute session lifetime shorter than the idle
one. Only SQLite is supported as the database.

## Deployment variables

These variables belong in `deploy/.env`. Compose uses them to select images, bind
mounts, ports, and the browser address.

| Variable                    | Meaning                                                                                            |
| --------------------------- | -------------------------------------------------------------------------------------------------- |
| `VALMIN_DOMAIN`             | Browser hostname or LAN IP, without a scheme, port, or path.                                       |
| `VALMIN_TLS`                | `'tls internal'` for local certificates; empty for public certificate issuance.                    |
| `VALMIN_HSTS`               | `Strict-Transport-Security` value; `max-age=31536000`. `max-age=0` turns it off.                   |
| `VALMIN_HOST_DATA_ROOT`     | Absolute data directory on the host; `/srv/valmin` in the example.                                 |
| `VALMIN_IMAGE`              | Panel image; `ghcr.io/valminhq/valmind@sha256:…` from the installer, `valmin/valmind:dev` after a local build. |
| `VALMIN_GAME_IMAGE`         | Game runtime image; `ghcr.io/valminhq/valheim@sha256:…` from the installer, `valmin/valheim:dev` after a local build. |
| `VALMIN_STEAMCMD_IMAGE`     | Download helper; defaults to `steamcmd/steamcmd:latest`, pinned by digest.                         |
| `VALMIN_SOCKET_PROXY_IMAGE` | Defaults to `tecnativa/docker-socket-proxy:v0.5.0`, pinned by digest.                              |
| `VALMIN_CADDY_IMAGE`        | Defaults to `caddy:2-alpine`, pinned by digest.                                                    |
| `VALMIN_HTTP_PORT`          | Host TCP port for HTTP redirects; defaults to `80`.                                                |
| `VALMIN_HTTPS_PORT`         | Host TCP and UDP port for HTTPS; defaults to `443`. Also requires an origin override when changed. |
| `VALMIN_SUBNET_PREFIX`      | First two octets of the three Docker subnets; defaults to `10.89`. Set before the first start.     |

Keep `.env` valid shell syntax: quote values containing spaces, and put no spaces
around `=`. `prepare-host.sh` reads the same file to select images. The default images
are pinned by digest in `compose.yaml` and move only when a release updates them; an
override should name a digest too.

## Daemon settings

| Daemon environment variable                 | Purpose / default                                                                  |
| ------------------------------------------- | ---------------------------------------------------------------------------------- |
| `VALMIN_SERVER_EXTERNAL_URL`                | Required browser origin: `http`/`https`, host, any nonstandard port, no path.      |
| `VALMIN_SERVER_LISTEN`                      | HTTP listen address; `:8080`.                                                      |
| `VALMIN_SERVER_TRUSTED_PROXIES`             | Trusted proxy CIDRs; empty by default. Compose sets Caddy's address.               |
| `VALMIN_DATA_ROOT`                          | Data path visible to the daemon; `/srv/valmin`.                                    |
| `VALMIN_DATA_HOST_ROOT`                     | Required absolute path to that same directory on the Docker host.                  |
| `VALMIN_DATA_FREE_SPACE_FLOOR_BYTES`        | Free space below which disk-heavy jobs are refused; `2147483648` (2 GiB).          |
| `VALMIN_DOCKER_ENDPOINT`                    | Docker API endpoint; `unix:///var/run/docker.sock`. Compose uses the socket proxy. |
| `VALMIN_GAME_IMAGE`                         | Runtime image already present on the Docker host.                                  |
| `VALMIN_GAME_STEAMCMD_IMAGE`                | Download helper image; `steamcmd/steamcmd:latest`.                                 |
| `VALMIN_GAME_NETWORK`                       | Docker network shared by game containers and the daemon; `valmin-games`.           |
| `VALMIN_GAME_PUBLIC_IP_URL`                 | Service that answers with the host's public IP; `https://api.ipify.org`. See [reachability](#check-that-players-can-reach-a-server). |
| `VALMIN_GAME_DEFAULT_MEM_MB`                | Default per-server memory limit; `4096`.                                           |
| `VALMIN_GAME_STOP_TIMEOUT`                  | Graceful stop timeout; `120s`, also the minimum.                                   |
| `VALMIN_GAME_LOG_PATTERNS`                  | Log-line pattern overrides; empty. See [log patterns](#override-log-patterns).     |
| `VALMIN_PORTS_BASE` / `VALMIN_PORTS_STRIDE` | Port allocation start and spacing; `2456` / `5`.                                   |
| `VALMIN_THUNDERSTORE_BASE_URL`              | Thunderstore host; `https://thunderstore.io`.                                      |
| `VALMIN_THUNDERSTORE_SYNC_INTERVAL`         | How often every enabled registry is refreshed; `1h`.                               |
| `VALMIN_HEXIUM_BASE_URL`                    | Hexium host; `https://valheim.hexium.gg`.                                          |
| `VALMIN_HEXIUM_ENABLED`                     | Whether Hexium is searched and installable; `true`.                                |
| `VALMIN_AUTH_SESSION_IDLE_TTL`              | Sign-out after this long without activity; `24h`.                                  |
| `VALMIN_AUTH_SESSION_ABSOLUTE_TTL`          | Sign-out after this long regardless of activity; `30d`.                            |
| `VALMIN_AUTH_INVITE_TTL`                    | How long an invite link stays valid; `7d`.                                         |
| `VALMIN_LOG_LEVEL` / `VALMIN_LOG_FORMAT`    | Log verbosity and format; `info` / `json`.                                         |

## Mod registries

The panel reads mods from two registries, and searches its own copy of their
indexes rather than calling out on each request. Both are on by default.

| Registry     | Default address             | Setting                                            |
| ------------ | --------------------------- | -------------------------------------------------- |
| Thunderstore | `https://thunderstore.io`   | Always enabled.                                    |
| Hexium       | `https://valheim.hexium.gg` | `VALMIN_HEXIUM_ENABLED`, `true` unless you set it. |

`VALMIN_THUNDERSTORE_SYNC_INTERVAL` paces the single refresh job that visits both;
there is no separate interval per registry. A run that fails for one registry still
records the other, and the panel keeps serving the last index it managed to store. An admin can
also start one from the **Mods** screen.

Set `VALMIN_HEXIUM_ENABLED=false` to run on Thunderstore alone. A disabled registry
disappears from search and cannot supply an install, but mods already installed from
it keep their name, version and deprecation notice, so turning it off never blanks
rows on the **Mods** screen. Nothing is uninstalled, and re-enabling it restores the
listings at the next refresh.

An enabled registry needs an absolute `base_url`. The daemon refuses to start on a
relative or empty one rather than failing a refresh every interval where nobody sees
it.

Both registries are read-only to the panel. It downloads packages and caches them; it
never uploads, mirrors, or republishes anything.

## Compose and data paths

Adding an arbitrary variable to `deploy/.env` does not pass it into the container.
Add daemon overrides to `valmind.environment` in Compose too. Compose's
`VALMIN_STEAMCMD_IMAGE` maps to the daemon's `VALMIN_GAME_STEAMCMD_IMAGE`.

Valmin checks the data path mapping on startup. If `/mnt/games/valmin` is mounted
at `/srv/valmin`, the host root is `/mnt/games/valmin` and the daemon root is
`/srv/valmin`.

## Check that players can reach a server

A server's **Check reachability** button needs the host's public IPv4 address. The panel asks
`game.public_ip_url` (`VALMIN_GAME_PUBLIC_IP_URL`) for it: an HTTP or HTTPS address of a service
that answers with the caller's address as plain text. The default is `https://api.ipify.org`.
That service, and Steam when a server has public listing on, learn the host's address from
the request; the game server already registers it with Steam. To use another service, set the
variable, which Compose passes to the daemon only through `deploy/compose.override.yaml`
(described below). To turn the lookup off, set `game: {public_ip_url: ""}` in a YAML config
file passed with `--config`: an empty environment variable is ignored, so it cannot switch the
lookup off. The check then reports the address as unknown, except for crossplay servers, which
need no address.

## Apply configuration changes

After changing deployment variables, run from `deploy/`:

```sh
docker compose config --quiet
docker compose up -d
```

Compose recreates services whose configuration changed. A restart alone does not
load changed environment variables. If you only edit the bind-mounted `Caddyfile`,
run `docker compose restart caddy` to load it.

For daemon settings that Compose does not pass through, create
`deploy/compose.override.yaml`. For example:

```yaml
services:
  valmind:
    environment:
      VALMIN_LOG_LEVEL: debug
      VALMIN_GAME_DEFAULT_MEM_MB: "6144"
```

Compose loads this file automatically when you run commands from `deploy/`.
The memory setting is the default for newly created servers; change an existing
server's limit through its settings. Keep local overrides out of source control
if they contain deployment-specific values.

## Use a different HTTPS port

For a LAN server with ports 80 and 443 already occupied, set these values in
`deploy/.env`:

```dotenv
VALMIN_DOMAIN=192.168.1.100
VALMIN_TLS='tls internal'
VALMIN_HTTP_PORT=8081
VALMIN_HTTPS_PORT=8443
```

Add the matching browser origin in `deploy/compose.override.yaml`, merging it with
any overrides already there:

```yaml
services:
  valmind:
    environment:
      VALMIN_SERVER_EXTERNAL_URL: https://192.168.1.100:8443
```

Apply the changes with `docker compose up -d`, then open
**https://192.168.1.100:8443**. Changing only the published port leaves the daemon
expecting port 443 and causes origin checks to fail. Do not append `:8443` to
`VALMIN_DOMAIN`: that would also change Caddy's listener inside the container.

Use the HTTPS URL directly. Caddy's default HTTP redirect does not know about the
host port remapping. Public certificate validation still requires the CA's standard
challenge ports to reach Caddy, or a separate DNS-validation setup.

## Override log patterns

Valmin reads game and BepInEx log lines to learn when a server is ready, when a save
has finished, how many players are connected, and which plugins loaded. The built-in
patterns match the lines from the game build they were measured on. If a game patch
changes one of those lines, you can replace its pattern until a Valmin release
supports the new line:

```yaml
game:
  log_patterns:
    ready: "Game server connected|Dedicated server ready"
```

The key is the event kind and the value is a
[Go regular expression](https://pkg.go.dev/regexp/syntax). The value replaces every
built-in pattern for that kind. If some servers still print the old line, include both
lines with `|`. From the environment, pass a JSON object:
`VALMIN_GAME_LOG_PATTERNS='{"ready":"Game server connected"}'`.

The kinds are `ready`, `save_complete`, `saved_zdos`, `quit`, `plugin_count`,
`plugin_loading`, `plugin_failed`, `crossplay_registered`, `crossplay_session`, `crossplay_ready`,
`player_count`, `connections`, `peer_joined`, `peer_left`, `disk_thresholds`,
`player_identity`, `platform_id`, `character` (a character spawning: its name in group 1 and
the connection's number in group 2), `peer_timeout`, and `valmin_eye` (a ValminEye event line,
its JSON object in the one capture group). Their built-in patterns are in
[the pattern source](../internal/instance/patterns.go).

The daemon does not start if an override names an unknown kind, does not compile,
matches an empty line, or has fewer capture groups than the built-in pattern. A pattern
that matched every line could, for example, make Valmin think a world save had finished
before it had. For the same reason, a `save_complete` override is refused if it matches
any of the earlier save steps the server logs, such as `World save writing finishing` or
`World save (1/5) Cloud & Backup checks done`: `World save` alone would fire before the
world is written. The daemon logs a warning at startup while overrides are set, and the
[support bundle](troubleshooting.md#support-bundle) includes them. Remove each
override after upgrading to a release that supports the new line.

## Files, secrets, and value formats

Each daemon setting also accepts a `_FILE` environment variable, whose value is a
path to a file containing the setting. For example, `VALMIN_SERVER_EXTERNAL_URL_FILE`
reads the URL from a file visible inside the panel container. Setting both the
direct variable and its `_FILE` form is an error.

`VALMIN_SECRETS_MASTER_KEY_FILE` names the encryption key itself; it defaults to
`secret.key` under the data root. It is different from the generic `_FILE` mechanism.
The panel generates the key only on its first start. Afterwards a missing or
different key stops startup; see [key recovery](operations.md#recover-from-a-lost-master-key).
The database defaults to `panel.db` under the same root. Preserve both in
[installation backups](operations.md#back-up-the-whole-installation).

Durations accept values such as `30s`, `24h`, and whole days such as `7d`.
Environment variables for lists, such as trusted proxy CIDRs, use comma-separated
values. `VALMIN_GAME_LOG_PATTERNS` takes a JSON object. Log levels are `debug`, `info`, `warn`, and `error`; formats are `json` and `text`.

See [the configuration source](../internal/config/config.go) for all settings and
defaults. Server settings, accounts, and schedules live in SQLite and are managed
through the panel.

## Remote backup transport

Remote destinations are configured through the admin UI/API. Deployment-only
settings are `remote_backups.rclone_binary` (default `rclone`),
`remote_backups.rclone_config` (default `<data.root>/rclone/rclone.conf`), and
`remote_backups.allowed_private_cidrs` (default empty). The daemon derives the
usual `VALMIN_REMOTE_BACKUPS_*` environment names.

Keep the rclone configuration directory persistent, owned by UID/GID 10000, and
writable for token refresh. Use mode 0700 for the directory and 0600 for its file.
See [remote backup setup](remote-backups.md).
