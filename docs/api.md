# HTTP API and WebSockets

[Documentation](README.md) / API

The panel uses a JSON API under `/api/v1`. Requests use the same HTTPS origin as
the browser UI. This guide covers the implemented authentication flow, common
server operations, and live subscriptions.

Authentication uses an opaque session token carried in a cookie. After password
verification, the server generates a random 256-bit token and stores its hash in
SQLite. Requests are checked against the stored session, expiry, and account
status; the cookie does not contain a client-selected user ID or role.

API keys and bearer-token authentication are
not implemented. The **Encryption keys** administration page manages encryption keys,
not API credentials.

## Sign in and read servers

The examples use `curl` and `awk`. Replace the hostname with the origin configured
in `VALMIN_SERVER_EXTERNAL_URL`.

Create a private directory for the example's credential and cookie files:

```sh
mkdir -m 700 valmin-api-session
cd valmin-api-session
umask 077
VALMIN_URL=https://valmin.example.com
```

Create `login.json` in that directory with your panel account credentials:

```json
{
  "username": "YOUR_USERNAME",
  "password": "YOUR_PASSWORD"
}
```

Sign in and save the returned cookies:

```sh
curl --fail-with-body --silent --show-error \
	--cookie-jar cookies.txt \
	--header 'Content-Type: application/json' \
	--data-binary @login.json \
	"$VALMIN_URL/api/v1/auth/login"
```

A successful login returns the user object and sets two cookies:

| Cookie           | Purpose                                                                                 |
| ---------------- | --------------------------------------------------------------------------------------- |
| `valmin_session` | Authenticates the account. Secure and HttpOnly.                                         |
| `valmin_csrf`    | Supplies the token for authenticated writes. Secure and readable by browser JavaScript. |

Read the servers your account can access:

```sh
curl --fail-with-body --silent --show-error \
	--cookie cookies.txt --cookie-jar cookies.txt \
	"$VALMIN_URL/api/v1/instances"
```

The response contains `items`, `next_cursor`, and `total`. An account with no
visible servers receives:

```json
{
  "items": [],
  "next_cursor": null,
  "total": null
}
```

Administrators see all servers. Members see servers for which they have a grant.
An inaccessible resource can return `404 not_found`, just like a missing resource.

## Read the scheduler timezone

`GET /api/v1/schedules` returns the collection fields `items`, `next_cursor`, and
`total`, plus a top-level `timezone`. The timezone is `UTC`, including when `items`
is empty. Use this field to label the time before creating the first schedule.
Individual schedule records also retain their `timezone` field. A `cron` value with
a `TZ=` or `CRON_TZ=` prefix is refused with `422`, so every schedule runs in that
timezone.

## Read upcoming scheduled runs

Every schedule returns `upcoming_runs`, the next 5 times it fires as RFC 3339 UTC
timestamps, earliest first. The list starts at `next_run_at` while that time is still
ahead. A held run keeps its past `next_run_at` until it starts, so the list skips it. A
disabled schedule returns an empty list.

## Hold scheduled runs for players

`restart` and `backup` schedules stop a running server, so they can wait for players to
leave. Set the policy on `POST /api/v1/schedules` or `PATCH /api/v1/schedules/{id}`:

| Field                  | Default | Meaning                                                            |
| ---------------------- | ------- | ------------------------------------------------------------------ |
| `wait_for_empty`       | `false` | Hold a due run while players may be connected.                     |
| `max_deferral_seconds` | `7200`  | Longest a due run is held, from `60` to `86400`.                   |
| `unknown_players`      | `wait`  | `wait` treats an unknown player count as occupied, `run` as empty. |

Every schedule also returns `deferred_since`, when the clock began holding the run, and
`deferred_until`, the latest it starts. Both are `null` unless a run is held.

`wait_for_empty: true` on any other kind returns `422` with `invalid` on `wait_for_empty`.
A `max_deferral_seconds` outside its range returns `422` with `out_of_range`, and an
`unknown_players` other than `wait` or `run` returns `422` with `invalid`.

Only a running server counts as occupied. The player count comes from the server log and is
unknown until the server reports it. A held run starts on the first check that finds the
server empty, or at `deferred_until` with players connected. Disabling or deleting the
schedule, changing its `cron`, or turning `wait_for_empty` off ends the hold. Re-enabling a
schedule moves `next_run_at` to the next time its `cron` matches, so a run missed while it
was disabled does not start. When a hold starts, ends or changes, the `instance.{id}.state`
topic sends `{"type":"maintenance","instance":"ID"}`; read `GET /api/v1/schedules` again
for the new state.

On a server with the `Tristan-ValheimRcon` mod, the panel also sends an RCON `say` warning
to players when a hold starts, and again 5 minutes before `deferred_until`. A hold with 5
minutes or less left when it starts gets only the first warning. A failed final warning is
retried each minute, and changing `max_deferral_seconds` during a hold sends a new final
warning for the new `deferred_until`. These warnings use the server's command rate limit
and are not written to the audit log.

## Make a state-changing request

Authenticated `POST`, `PUT`, `PATCH`, and `DELETE` requests need the session cookie
and an `X-CSRF-Token` header containing the `valmin_csrf` value. Read the current
value from the cookie jar:

```sh
VALMIN_CSRF=$(awk '$6 == "valmin_csrf" { print $7 }' cookies.txt)
VALMIN_INSTANCE_ID=REPLACE_WITH_INSTANCE_ID
```

Start a stopped server:

```sh
curl --fail-with-body --silent --show-error \
	--cookie cookies.txt \
	--header "X-CSRF-Token: $VALMIN_CSRF" \
	--request POST \
	"$VALMIN_URL/api/v1/instances/$VALMIN_INSTANCE_ID/start"
```

A script can omit `Origin`. If it sends `Origin`, the value must match the panel's
configured origin exactly. Cross-origin browser access is not supported. WebSocket
connections always require a matching `Origin`.

A new login changes the CSRF token. Authenticated reads reissue the CSRF cookie,
so keep the cookie jar updated. Sign in again after a session expires; the default
idle timeout is 24 hours and the absolute lifetime is 30 days.

## Follow asynchronous jobs

Server operations such as creation, start, stop, backup, and restore return
`202 Accepted` with a `job_id`. The `Location` response header points to
`/api/v1/jobs/{job_id}`. Acceptance means the job was recorded, not that it succeeded.

```sh
VALMIN_JOB_ID=REPLACE_WITH_JOB_ID
curl --fail-with-body --silent --show-error \
	--cookie cookies.txt \
	"$VALMIN_URL/api/v1/jobs/$VALMIN_JOB_ID"
```

The job includes `status`, `progress`, and timestamps. It can also include
`message`, `error_code`, and `error`. Terminal statuses are `succeeded`, `failed`,
and `cancelled`. Reading a failed job still returns HTTP 200: inspect its status.

A job also carries three fields that say who started it and what it changed. They
appear on `GET /api/v1/jobs/{id}`, on each item of `GET /api/v1/instances/{id}/jobs`,
and in the body of a `202` response:

| Field               | Meaning                                                                                                                     |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `requested_by_name` | Username of the person who started the job. Omitted for system work, scheduled runs, and users since deleted.               |
| `scheduled`         | `true` when a schedule started the job. Always present, and still `true` after the schedule is deleted.                     |
| `changes`           | Mod jobs only, omitted for every other kind. The packages the job was asked to change, as `action` and `full_name` entries. |

`requested_by_name` is filled in when a job is read, so the `202` response leaves it
out. Anyone who can view the server can read the names of the people who started its
jobs.

Each entry of `changes` has `action`, one of `install`, `update`, `uninstall`,
`enable`, or `disable`, and `full_name`. `from_version` and `to_version` appear where the
job's stored request carries them: an update gives both, a single install gives
`to_version`, and an install whose version is a minimum gives neither. The list is read
from the job's stored request for `mod_install` (a single install or an update of every
mod), `mod_uninstall`, and `mod_toggle`.

A conflicting operation returns `409 job_in_progress`. Read the existing job or
server state before retrying a request whose response was lost. Do not assume a
network timeout means the operation did not start.

Jobs that archive or replace world data (import, world restore and delete, backup
restore, clone, and game or mod updates) also ask Docker whether the game container
is running. If it was started outside the panel, the job fails with `error_code`
`instance_must_be_stopped` and leaves the world unchanged.

`POST /api/v1/jobs/{job_id}/cancel` requests cancellation and returns 204. Cancellation
is cooperative and can be refused with `409 job_not_cancellable` after a job
passes its cancellation point.

`GET /api/v1/instances/{id}/jobs` lists a server's jobs, newest first, with the usual
`cursor` and `limit` pagination. It needs `instance.view` and nothing more. Add
`scheduled=true` to list only the runs a schedule started. A value that is not a boolean
returns `400 invalid_parameter`. A skipped scheduled run, such as one that found another
job holding the server, is recorded as `cancelled` with an `error_code` and an `error`
that explains why. A job an operator cancelled has no `error_code`.

## Common endpoints

Paths in this table are relative to `/api/v1`. Each operation checks the account's
permissions as well as the server's current state. IDs in braces are path parameters.

| Method   | Path                                     | Result or purpose                                                     |
| -------- | ---------------------------------------- | --------------------------------------------------------------------- |
| `GET`    | `/auth/me`                               | Current account.                                                      |
| `POST`   | `/auth/logout`                           | Revoke the session and clear its cookies.                             |
| `GET`    | `/me/permissions`                        | Current account's permissions.                                        |
| `POST`   | `/me/password`                           | Change your own password; returns `204`.                              |
| `GET`    | `/game/options`                          | Launch options and validation limits; any signed-in account.          |
| `GET`    | `/instances`                             | Visible servers.                                                      |
| `POST`   | `/instances`                             | Provision a server; returns a job.                                    |
| `GET`    | `/instances/{id}`                        | Server settings and current state.                                    |
| `PATCH`  | `/instances/{id}`                        | Update supplied settings.                                             |
| `GET`    | `/instances/{id}/capabilities`           | Available server capabilities.                                        |
| `POST`   | `/instances/{id}/commands`               | Send one console command over RCON; returns the reply.                |
| `GET`    | `/instances/{id}/password`               | Game password. Needs `instance.view`; every read is audited.          |
| `POST`   | `/instances/{id}/start`                  | Start a server; returns a job.                                        |
| `POST`   | `/instances/{id}/stop`                   | Stop a server gracefully; returns a job.                              |
| `POST`   | `/instances/{id}/restart`                | Restart a server; returns a job.                                      |
| `POST`   | `/instances/{id}/acknowledge`            | Re-check a server parked in `error`; needs start permission, audited. |
| `GET`    | `/instances/{id}/update-status`          | Game update availability.                                             |
| `POST`   | `/instances/{id}/update`                 | Update the game; returns a job.                                       |
| `GET`    | `/instances/{id}/logs`                   | Recent game logs.                                                     |
| `GET`    | `/instances/{id}/stats`                  | Current resource sample. Unknown values can be null.                  |
| `GET`    | `/instances/{id}/jobs`                   | Server job history; `scheduled=true` lists scheduled runs only.       |
| `GET`    | `/instances/{id}/backups`                | World backup catalog.                                                 |
| `POST`   | `/instances/{id}/backups?mode=quiesced`  | Stop, back up, and resume a previously running server; returns a job. |
| `POST`   | `/instances/{id}/backups?mode=hot`       | Best-effort backup without stopping; returns a job.                   |
| `GET`    | `/instances/{id}/backups/{bid}/download` | Download an archive.                                                  |
| `POST`   | `/instances/{id}/backups/{bid}/restore`  | Restore into a stopped server; returns a job.                         |
| `GET`    | `/instances/{id}/worlds`                 | Worlds in the server's save directory.                                |
| `POST`   | `/instances/{id}/worlds/{name}/restore`  | Load another world already on disk; returns a job.                    |
| `DELETE` | `/instances/{id}/worlds/{name}`          | Delete a world from a stopped server; returns a job.                  |
| `GET`    | `/mods/search`                           | Search the cached catalogue across registries.                        |
| `GET`    | `/mods/{namespace}/{name}`               | One catalogue package and its version history.                        |
| `GET`    | `/instances/{id}/mods`                   | Installed mods.                                                       |
| `POST`   | `/instances/{id}/mods/resolve`           | Preview the dependency closure an install would apply.                |
| `POST`   | `/instances/{id}/mods`                   | Install a mod and its dependencies; returns a job.                    |
| `DELETE` | `/instances/{id}/mods/{full_name}`       | Uninstall a mod; returns a job.                                       |
| `PATCH`  | `/instances/{id}/mods/{full_name}`       | Change a mod's client tag or lock, or enable or disable it.           |
| `POST`   | `/instances/{id}/mods/updates/resolve`   | Preview updating every mod that has a newer version.                  |
| `POST`   | `/instances/{id}/mods/updates`           | Back up the world, then apply those updates; returns a job.           |
| `GET`    | `/instances/{id}/mods/export`            | Client manifest preview, or the archive with `format=r2z`.            |
| `GET`    | `/instances/{id}/manifest`               | Server definition: settings, pinned mods, and config files.           |
| `POST`   | `/instances/manifest/preview`            | Check a server definition before importing it.                        |
| `POST`   | `/instances/import`                      | Create a server from a definition; returns a job.                     |
| `GET`    | `/instances/{id}/configs`                | Available configuration files.                                        |
| `GET`    | `/instances/{id}/configs/{file}/raw`     | Raw configuration with an `ETag` header.                              |
| `PUT`    | `/instances/{id}/configs/{file}/raw`     | Replace raw configuration on a stopped server; requires `If-Match`.   |
| `GET`    | `/jobs/{id}`                             | Job status and result.                                                |
| `POST`   | `/jobs/{id}/cancel`                      | Request cancellation.                                                 |

### Create a server

Send JSON to `POST /api/v1/instances` with `Content-Type: application/json`, session
cookies, and the CSRF header. For example:

```json
{
  "name": "Friends",
  "server_name": "Friends server",
  "world_name": "Meadows",
  "password": "replace-this-password",
  "public": false,
  "crossplay": false,
  "mem_limit_mb": 4096,
  "start_after_provision": false
}
```

`name` is the panel label; `server_name` is the game's server name. The password
must have at least five characters and must not occur inside the server or world
name. Use `/game/options` for the launch vocabulary and limits of this build.

Optional fields include `preset`, `modifiers`, `cpu_limit`, and `mods`.
Each mod selection contains `full_name` and `version`. Creation is administrator-only.
See the [create request definition](../internal/api/provision.go) for the full shape.

### Mods and registries

Packages come from more than one registry, so a mod is identified by `full_name`
**and** `source`. Two registries can publish different files under one name and
version, and both fields are needed to name a package unambiguously.

`source` is `thunderstore` or `hexium`. A name outside that set is rejected — `400`
on a query parameter, `422` on a request body — rather than silently ignored.

`GET /mods/search` accepts `q`, `category`, `source`, `cursor`, and `limit`. Omitting
`source` searches every enabled registry; there is no `all` value. A package both
registries publish returns **one item per registry**, each with its own
`latest_version`, so `full_name` is not unique within a page.

```json
{
  "items": [
    {
      "full_name": "denikson-BepInExPack_Valheim",
      "source": "thunderstore",
      "namespace": "denikson",
      "name": "BepInExPack_Valheim",
      "latest_version": "5.4.2202",
      "downloads": 41000000,
      "is_deprecated": false,
      "categories": ["Libraries"],
      "icon_url": "https://example.invalid/icon.png"
    }
  ],
  "next_cursor": null,
  "synced_at": "2026-09-21T12:00:00Z",
  "registries": [
    { "source": "thunderstore", "enabled": true, "synced_at": "2026-09-21T12:00:00Z" },
    { "source": "hexium", "enabled": true, "synced_at": null }
  ]
}
```

`registries` reports every registry the build knows, whether it is enabled, and when
it last refreshed — including a disabled one, so a client can tell "switched off"
from "never downloaded". The top-level `synced_at` is the oldest refresh among the
enabled registries this search covered, and is `null` while any of them has never
refreshed, so a partial catalogue is never reported as fully fresh. Read `registries`
to say which part is missing.

`GET /mods/{namespace}/{name}` takes an optional `source`. Without it, the package is
returned from whichever enabled registry carries it, preferring Thunderstore; with
it, a registry that does not carry the package is a `404` rather than a fallback to
the other one. The `versions` array holds only that registry's versions, newest first. A
disabled registry is absent from both catalogue endpoints.

`POST /instances/{id}/mods` and `POST /instances/{id}/mods/resolve` take
`{"full_name": ..., "version": ..., "source": ...}`. The same three fields appear in
the `mods` array of a create-server request. `source` is optional and defaults to no
preference, but sending the value from the catalogue row is what guarantees the bytes
you saw are the bytes installed.

`version` is exact. Any version from `GET /mods/{namespace}/{name}` can be installed,
and a version below the installed one is a downgrade. When the request names a `source`,
the requested package comes from that registry only. The dependencies of the chosen
version are minimums: a dependency already installed at that version or newer is left
alone. A downgrade changes mod files only. It does not restore the world or config
files, so the world backup taken first is the way back.

Dependency identifiers carry no registry, so a closure can span registries. Each node
of a resolve response reports the `source` it resolved from: an already-installed
package keeps the registry its files came from, otherwise the requested registry is
preferred, otherwise whichever enabled registry carries that version. A disabled
registry never supplies a resolution, which is a `409`.

Each node also has `from_version`, the installed version, empty when the package is not
installed, and `change`: `none`, `install`, `upgrade`, or `downgrade`. The response also
has `removals` (installed packages the change uninstalls), `kept` (modpack members it
leaves at another version, with a `reason`), and `conflicts`. A conflict is a dependency
the change would leave unmet: `full_name` at `version` needs `dependency` at `requires`
or newer, and the change leaves it at `have`, or removes it when `have` is empty.
`locked` says whether that dependency is locked. The install job refuses a change with
conflicts (`mod_conflict`), so resolve first and fix them. Only conflicts the change
causes are reported. The response's `backup` is `true` when the install would move or
remove an installed package on a server that has a world. The install job then backs up
the world first, as **Update all** does, and keeps the backup even if the install fails.

A modpack is a package its registry files under the `Modpacks` category. Its dependencies
are its members, each pinned to one version. Installing a modpack at another version
applies that version to the members:

- A member at the version the installed modpack pins follows the modpack to its new
  pin, up or down.
- A member that is locked, or that was installed by hand, keeps its version. It is
  listed in `kept` when that differs from the new pin.
- Any other installed member is raised to the new pin if it is older, and kept if it is
  newer.
- A member the new version drops is removed when it is still at the old pin, was
  installed with the modpack, and nothing else needs it. Otherwise it is kept and listed
  in `kept` with an empty `pack_version`.

Mods outside the modpack change only as they would for any install: when a member needs
a newer version of one, and BepInEx, which an install raises to its newest version. The
modpack's own dependencies are not reported as conflicts, since a kept member differs
from the pin on purpose.

`GET /instances/{id}/mods` reports each installed mod's `source` and an
`update_version`, which is empty unless a strictly newer version exists **in the
registry the mod was installed from**. Installing the same mod from a second registry
into one server is not possible; uninstall it first. Each row also has `enabled`, and a
`load_status` of `loaded`, `not_seen`, `failed`, or `null`. When the status is `failed`,
`load_error` carries the mod loader's own message. `not_indexed` is `true` for a mod its
registry no longer lists: the last complete refresh of that registry's catalogue did not
include it. Such a row has an empty `update_version` and is left out of **Update all**.
It stays `false` until the registry has completed at least one refresh. `file_count` counts
every file the mod placed and `config_file_count` those under `BepInEx/config/`. Uninstall
keeps the config files, including edited settings. `locked` is `true` for a locked mod and
`is_pack` for a modpack. `pack` names the installed modpack that includes the mod and
`pack_version` the version it pins; both are empty outside a modpack. `pack_override` is
`true` when the mod no longer follows its modpack.

`PATCH /instances/{id}/mods/{full_name}` with `{"side": ...}` edits the client-requirement
tag and answers the row. `{"locked": true}` locks the mod at its installed version and
`{"locked": false}` unlocks it; both answer the row, work while the server runs, and are
written to the audit log as `instances.mods.lock` or `instances.mods.unlock`. **Update
all** skips a locked mod. An install that needs a locked mod at another version reports
a conflict instead of moving it. Resolving a locked mod at another version answers
`409 mod_conflict` with `details.locked`; installing it is accepted, and the job fails
with `mod_conflict`. A lock applies to changes planned after it is set, not to a mod
change already running. With `{"enabled": false}` or `{"enabled": true}` it moves the
mod's files out of or back into the server and returns a job. Send `enabled` on its own,
and stop the server first. A request that matches the mod's current state answers the
row. Disabling is refused (`409 mod_conflict`) for BepInEx itself and while an enabled
mod depends on this one (`details.required_by`). Enabling is refused while this mod
depends on a disabled one (`details.disabled`). Installs and updates whose dependency
closure includes a disabled mod are refused the same way.

`POST /instances/{id}/mods/updates/resolve` takes no body and returns `targets` (each
mod with a newer version in its own registry), `nodes` (every package that would
change, with `from_version` empty for a new dependency), `conflicts`, and `backup`.
Locked mods, modpacks, and members that follow their modpack are never targets; update
a modpack by installing its new version. To apply, send `{"targets": [...]}` with the
targets from the preview to `POST /instances/{id}/mods/updates`. The job backs up the
world, then updates all targets together, rolling all of them back if one fails. It is
refused while the preview has conflicts.

### Server definitions

`GET /instances/{id}/manifest` exports a server definition. Each entry in its `mods`
array has `full_name`, `source`, `version`, and `side`. `source` is the registry the
installed files came from. `POST /instances/manifest/preview` checks a definition, and
`POST /instances/import` creates a server from one. On import, a mod that names a
`source` installs from that registry only, and the preview reports it as unavailable if
that registry does not carry the version. A definition without `source` installs from
whichever registry carries the version. The import installs each modpack before the
other mods, so the mods it bundles arrive as its members, and it never lowers a mod that
an earlier mod in the definition already raised.

### Send server commands

`GET /instances/{id}/capabilities` reports `command_channel`: `rcon` when the server has
the `Tristan-ValheimRcon` mod, otherwise `none`. `allowed_commands` lists the commands
non-administrators may send.

`POST /instances/{id}/commands` with `{"command": "save"}` needs `commands.send` and a
running server, and returns `{"accepted": true, "output": "..."}` with the mod's reply.

| Error                   | Cause                                                               |
| ----------------------- | ------------------------------------------------------------------- |
| `409 unsupported`       | The server does not have the mod.                                   |
| `409 invalid_state`     | The server is not running.                                          |
| `422 validation_failed` | Empty, multi-line, over 1,024 bytes, or not allowed for the caller. |
| `429 rate_limited`      | Over 30 commands a minute per server, after a burst of five.        |
| `503 unavailable`       | The RCON connection failed.                                         |

### Edit settings and files

Omitted fields in a settings `PATCH` remain unchanged. Unknown JSON fields are
rejected. Some launch changes set `restart_required`; saving settings does not
mean the running game has adopted them.

The game password is never part of a server's JSON. Read it with
`GET /instances/{id}/password`, which returns `{"password": "..."}` and writes an
`instances.password.read` audit entry on each call. After a password change, a
running server keeps the previous password until it restarts.

For raw configuration replacement, first fetch the file and retain its `ETag`.
Send that exact value as `If-Match` with the replacement text and
`Content-Type: text/plain`. A missing or stale
value returns `412 stale_write`; reload and reconcile the file before retrying.

Other implemented API groups cover mods, schedules, grants, invitations, users,
player lists, webhooks, and recovery. Their request definitions are in
[the HTTP handlers](../internal/api), with usage examples in
[the frontend API clients](../web/src/lib/api). The table above is a common-operation
reference, not a complete schema for every route.

### Notifications and alert rules

Three events are sent to every enabled webhook without any configuration:
`instance_down` (a server stopped on its own), `update_available` (a new public game
build), and `backup_failed`. Alert rules (`/api/v1/admin/alert-rules`) route a condition kind
to chosen destinations and send `alert_opened` and `alert_resolved`. Alerts held during a
rule's quiet hours are sent when the window ends. A rule sends `alert_resolved` only for an
alert it opened.

`stale_backup` opens when a backup schedule goes `stale_factor` times its interval (2 by
default) without a consistent archive; a hot copy does not count. On a server that backs up
on restart, its restart schedule counts too, while its latest restart succeeded. A failed
restart raises `job_failed` instead.

When a rule covers the same incident as one of the three events, the rule's destinations
receive only the rule's alert. A failed backup is covered by a `job_failed` rule, a new
build by an `update_available` rule, and an unexpected stop by a `crash_loop` or
`instance_error` rule when this stop opens that condition. If the rule's condition is
already open, the rule sends nothing new, so its destinations still receive the event.
Destinations no rule names still receive every event.

Thresholds live in `params`: `crash_count`, `crash_window_seconds` and
`stuck_after_seconds` must be 0 or more, and `stale_factor` must be 0 or above 1. Zero or an
omitted field means the default; a value outside these ranges, or a duration too long to hold,
returns `422` with `out_of_range` on `params.<field>`. Sent `params` replace the stored ones.
Quiet hours (`quiet_start_minutes`, `quiet_end_minutes`, minutes from midnight, and
`quiet_timezone`) are sent together; an empty `quiet_timezone` clears them. Alerts still open
when quiet hours end are sent then; one that opens and clears inside the window is not sent.
On a `PATCH`, an empty `instance_id` clears the rule's server and a `null` one is ignored.
Creating, changing and deleting a rule writes an audit log entry with action
`panel.settings` and the operation `alert_rule_create`, `alert_rule_update` or
`alert_rule_delete`. Deleting a rule that does not exist returns `404`. A `low_disk` rule
is host-wide: naming an `instance_id` on one returns `422`.

### Grants

A grant gives one member a role on one server, plus optional extra capabilities. These
routes need `grants.manage`, which only administrators hold; everyone else gets `404`.
Paths are relative to `/api/v1`.

| Method   | Path                               | Purpose                                                             |
| -------- | ---------------------------------- | ------------------------------------------------------------------- |
| `GET`    | `/instances/{id}/grants`           | Every stored grant on the server, expired ones included.            |
| `GET`    | `/instances/{id}/grants/{user_id}` | One grant, with its `ETag`.                                         |
| `PUT`    | `/instances/{id}/grants/{user_id}` | Create or replace a grant; `201` when created, `200` when replaced. |
| `DELETE` | `/instances/{id}/grants/{user_id}` | Revoke a grant; needs `If-Match`.                                   |

The list also returns `roles`, each with the actions it carries, and
`extra_capabilities`, each with a `risk` sentence, so a client can offer them without
naming roles itself. `PUT` takes `{"role": "viewer" | "operator", "perms": [...]}`. Send
`If-None-Match: *` to create only, or `If-Match` with the grant's `ETag` to replace. A
stale precondition returns `412 stale_write`, and a missing or malformed one returns
`400 invalid_parameter`. Every write is recorded in the audit log.

A `PUT` for an administrator is accepted. The store checks only that the user and the
server exist, and an administrator is allowed everything before any grant is read, so the
row is stored and has no effect. The Panel access page marks such a grant.

### Audit log

Only administrators can read the audit log; everyone else gets `404`. Entries are kept
permanently and record the actor's and server's names as they were when the entry was
written, so renaming or deleting either does not change history.

| Method | Path                | Purpose                                                        |
| ------ | ------------------- | -------------------------------------------------------------- |
| `GET`  | `/audit`            | Entries, newest first, with the usual cursor pagination.       |
| `GET`  | `/audit/filters`    | The actions, actors and servers that appear in the log.        |
| `GET`  | `/audit/export`     | Every entry matching the filters, as `audit-log.csv`.          |

`/audit` and `/audit/export` accept `action`, `user_id`, `instance_id`, `since` (inclusive) and
`until` (exclusive), the last two as RFC 3339 timestamps. Any other parameter, or a timestamp
that does not parse, returns `400 invalid_parameter`. `/audit/filters` lists deleted users and
servers too, each under the last name recorded for it.

Each entry carries `outcome`: `succeeded`, `failed`, `cancelled` or `requested`. An entry for
work that runs as a job links to it with `job_id` and takes its outcome from the job, with
`job_error` set when the job failed. An entry for a direct action, such as an RCON command, is
written before the action runs and updated when it ends. Entries written before outcomes were
recorded have `outcome` of `null`. `detail` is a JSON string for entries written by the
current version, for example the old and new value of each changed setting or the versions a
mod moved between; secret values, such as the game password, are recorded as changed but never
stored. Older entries may hold plain text.

Actions written from a request include `instances.start`, `instances.stop`, `instances.restart`,
`instances.delete`, `instances.create`, `instances.game.update`, `instances.backups.create`,
`instances.backups.restore`, `instances.backups.delete`, `instances.worlds.import`,
`instances.worlds.restore`, `instances.worlds.delete`, `instances.mods.install`,
`instances.mods.update`, `instances.mods.uninstall`, `instances.mods.enable`,
`instances.mods.disable`, `instances.mods.lock`, `instances.mods.unlock`,
`instances.settings.update`, `instances.configs.write`, `instances.commands.send`,
`schedules.create`, `schedules.update`, `schedules.delete` and `jobs.cancel`. Scheduled runs
and jobs the panel resumes after a restart are not entered, because nobody requested them.

## Errors and collection responses

API errors use this envelope:

```json
{
  "error": {
    "code": "not_found",
    "message": "The requested item could not be found.",
    "request_id": "example-request-id"
  }
}
```

Some errors also include `error.details`. Use `code` for branching and `message`
for display. Include `request_id` when investigating the matching daemon log.

| HTTP status | Common meaning                                                     |
| ----------- | ------------------------------------------------------------------ |
| 400         | Invalid JSON or query parameter.                                   |
| 401         | Missing/expired session or invalid credentials.                    |
| 403         | Missing permission, wrong origin, or failed CSRF check.            |
| 404         | Resource is missing or not visible to the account.                 |
| 409         | Conflicting job, invalid state, or operation unavailable.          |
| 412         | File changed since it was read, or required `If-Match` is missing. |
| 422         | Field validation failed.                                           |
| 429         | Rate limited; observe `Retry-After` when present.                  |
| 503         | Setup is incomplete or the service is unavailable.                 |

Paginated collections accept `cursor` and `limit`; the default limit is 50 and the
maximum is 200. Treat cursors as opaque. `next_cursor: null` marks the end, and
`total: null` means no count was supplied. Not every collection is paginated;
`GET /instances`, for example, returns all visible instances in one page.

## WebSockets

Connect to `wss://your-host/api/v1/ws?csrf=TOKEN`, passing the session cookie and
setting `Origin` to the configured HTTPS origin. `TOKEN` is the URL-encoded CSRF
cookie value. The socket provides events; use HTTP for state-changing operations.

Subscribe with a JSON text message:

```json
{
  "type": "subscribe",
  "topics": ["instance.INSTANCE_ID.state", "job.JOB_ID"]
}
```

Available topics:

| Topic                   | Payload                                         |
| ----------------------- | ----------------------------------------------- |
| `instance.{id}.console` | Game log lines.                                 |
| `instance.{id}.stats`   | Resource samples.                               |
| `instance.{id}.state`   | Server state changes and `maintenance` signals. |
| `job.{id}`              | Job progress and status.                        |

The server checks permission per topic and replies with `subscribed` or `error`.
To stop listening, send `{"type":"unsubscribe","topics":["job.JOB_ID"]}`.

After subscribing to a job, fetch its HTTP resource so a job that already finished
is not missed. Fetch current state again after reconnecting. Console replay is
bounded, and console/stat streams can lose samples under load. Do not treat the
socket as a durable event log. See the [wire message types](../internal/ws/message.go)
for payload fields.

## Health and public status

These routes are outside `/api/v1` and do not require a session:

| Method and path           | Response                                                                                                      |
| ------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `GET /healthz`            | HTTP 200 with `ok` when the daemon can respond.                                                               |
| `GET /readyz`             | JSON with `ready` and `components`; HTTP 503 if draining, SQLite is unavailable, or Docker cannot be reached. |
| `GET /public/status/{id}` | Server status only when its public status page is enabled.                                                    |

## Diagnostics

An account with panel administration permission can read the daemon's health report and
download a support bundle. Paths are relative to `/api/v1`.

| Method | Path                        | Result or purpose                                                      |
| ------ | --------------------------- | ---------------------------------------------------------------------- |
| `GET`  | `/admin/diagnostics`        | Health report: one entry per check, with its status, source, and time. |
| `GET`  | `/admin/diagnostics/bundle` | Redacted support bundle, as a zip archive.                             |
| `POST` | `/admin/diagnostics/run`    | Repeat the checks that need a container; returns a job.                |

Each check reports `status` as `ok`, `warn`, `fail`, or `unknown`, and `source` as `live`,
`startup`, `job`, or `config`. `unknown` means the check has no measurement behind it;
treat it as a missing answer rather than a pass. A failing check can also carry
`diagnostic`, the verbatim output of whatever was probed. That field is present in the API
response and absent from the support bundle, which omits filesystem paths.

Server entries include nullable `exit_code`, `oom_killed`, `restart_count`, and
`finished_at` values, plus `server_free_bytes` from the last game telemetry sample.
`mods_error` and `inspection_error` explain failed reads; clients must not interpret
the associated zero or empty values as successful measurements. The bundle replaces
these errors with generic messages.

`failed_jobs` lists the latest failed terminal job for each server and operation type,
including global jobs. Each item contains `id`, `kind`, and an optional `instance_id`.
Fetch `/jobs/{id}` for details.

Registry checks include reachability and a separate `.sync` check for each enabled
registry. Disabled registries report their configuration without making a request.

These three routes answer 403 to an account without the permission, not 404.

## Change your password

`POST /api/v1/me/password` changes the password of the signed-in account. It needs the
session cookie and the CSRF header. It takes no user id, so a caller can change only
their own password.

```json
{
  "current_password": "YOUR_CURRENT_PASSWORD",
  "new_password": "YOUR_NEW_PASSWORD"
}
```

Success is `204` with no body. Every other session of the account is removed and the
WebSockets those sessions held are closed. The session that made the request keeps
working: its cookie and CSRF token stay valid and its sockets stay open. An
administrator's password reset, by contrast, ends every session of the account,
including the target's own.

| Status | Code                  | Cause                                                                                       |
| ------ | --------------------- | ------------------------------------------------------------------------------------------- |
| `401`  | `invalid_credentials` | The current password is wrong. The message is `The current password is incorrect.`          |
| `401`  | `unauthenticated`     | There is no valid session.                                                                  |
| `422`  | `validation_failed`   | `current_password` is `required`, or `new_password` is `required` or `too_short` (under 8). |
| `429`  | `rate_limited`        | More than 5 attempts in a minute for this account. Wait for the `Retry-After` header.       |

The limit is 5 attempts per minute per user, with a burst of 5. It belongs to the
account, not to the client address, and it is not configurable. Every attempt that
passes field validation counts, whether or not the current password was right; a `422`
does not.

A successful change writes an audit log entry with action `users.password.change`, the
detail `{"target_user_id": "USER_ID"}` for the caller's own id, and outcome `succeeded`.
Neither password is stored in the entry, and a refused request writes none.

## End the session

```sh
curl --fail-with-body --silent --show-error \
	--cookie cookies.txt --cookie-jar cookies.txt \
	--header "X-CSRF-Token: $VALMIN_CSRF" \
	--request POST \
	"$VALMIN_URL/api/v1/auth/logout"
rm login.json cookies.txt
```

Logout returns 204 and invalidates the session. Remove the example's local files
when finished; they contain credentials.
