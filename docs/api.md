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
not implemented. The **Keys** administration page manages encryption keys, not
API credentials.

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
Individual schedule records also retain their `timezone` field.

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

A conflicting operation returns `409 job_in_progress`. Read the existing job or
server state before retrying a request whose response was lost. Do not assume a
network timeout means the operation did not start.

`POST /api/v1/jobs/{job_id}/cancel` requests cancellation and returns 204. Cancellation
is cooperative and can be refused with `409 job_not_cancellable` after a job
passes its cancellation point.

## Common endpoints

Paths in this table are relative to `/api/v1`. Each operation checks the account's
permissions as well as the server's current state. IDs in braces are path parameters.

| Method   | Path                                     | Result or purpose                                                     |
| -------- | ---------------------------------------- | --------------------------------------------------------------------- |
| `GET`    | `/auth/me`                               | Current account.                                                      |
| `POST`   | `/auth/logout`                           | Revoke the session and clear its cookies.                             |
| `GET`    | `/me/permissions`                        | Current account's permissions.                                        |
| `GET`    | `/game/options`                          | Supported launch options and validation limits.                       |
| `GET`    | `/instances`                             | Visible servers.                                                      |
| `POST`   | `/instances`                             | Provision a server; returns a job.                                    |
| `GET`    | `/instances/{id}`                        | Server settings and current state.                                    |
| `PATCH`  | `/instances/{id}`                        | Update supplied settings.                                             |
| `GET`    | `/instances/{id}/capabilities`           | Available server capabilities.                                        |
| `POST`   | `/instances/{id}/start`                  | Start a server; returns a job.                                        |
| `POST`   | `/instances/{id}/stop`                   | Stop a server gracefully; returns a job.                              |
| `POST`   | `/instances/{id}/restart`                | Restart a server; returns a job.                                      |
| `GET`    | `/instances/{id}/update-status`          | Game update availability.                                             |
| `POST`   | `/instances/{id}/update`                 | Update the game; returns a job.                                       |
| `GET`    | `/instances/{id}/logs`                   | Recent game logs.                                                     |
| `GET`    | `/instances/{id}/stats`                  | Current resource sample. Unknown values can be null.                  |
| `GET`    | `/instances/{id}/jobs`                   | Server job history.                                                   |
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
| `PATCH`  | `/instances/{id}/mods/{full_name}`       | Change a mod's client tag, or enable or disable it (returns a job).   |
| `POST`   | `/instances/{id}/mods/updates/resolve`   | Preview updating every mod that has a newer version.                  |
| `POST`   | `/instances/{id}/mods/updates`           | Back up the world, then apply those updates; returns a job.           |
| `GET`    | `/instances/{id}/mods/export`            | Client manifest preview, or the archive with `format=r2z`.            |
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
the other one. The `versions` array holds only that registry's versions. A disabled
registry is absent from both catalogue endpoints.

`POST /instances/{id}/mods` and `POST /instances/{id}/mods/resolve` take
`{"full_name": ..., "version": ..., "source": ...}`. The same three fields appear in
the `mods` array of a create-server request. `source` is optional and defaults to no
preference, but sending the value from the catalogue row is what guarantees the bytes
you saw are the bytes installed.

Dependency identifiers carry no registry, so a closure can span registries. Each node
of a resolve response reports the `source` it resolved from: an already-installed
package keeps the registry its files came from, otherwise the requested registry is
preferred, otherwise whichever enabled registry carries that version. A disabled
registry never supplies a resolution, which is a `409`.

`GET /instances/{id}/mods` reports each installed mod's `source` and an
`update_version`, which is empty unless a strictly newer version exists **in the
registry the mod was installed from**. Installing the same mod from a second registry
into one server is not possible; uninstall it first. Each row also has `enabled`, and a
`load_status` of `loaded`, `not_seen`, `failed`, or `null`. When the status is `failed`,
`load_error` carries the mod loader's own message. `not_indexed` is `true` for a mod its
registry no longer lists: the last complete refresh of that registry's catalogue did not
include it. Such a row has an empty `update_version` and is left out of **Update all**.
It stays `false` until the registry has completed at least one refresh.

`PATCH /instances/{id}/mods/{full_name}` with `{"side": ...}` edits the client-requirement
tag and answers the row. With `{"enabled": false}` or `{"enabled": true}` it moves the
mod's files out of or back into the server and returns a job. Send `enabled` on its own,
and stop the server first. A request that matches the mod's current state answers the
row. Disabling is refused (`409 mod_conflict`) for BepInEx itself and while an enabled
mod depends on this one (`details.required_by`). Enabling is refused while this mod
depends on a disabled one (`details.disabled`). Installs and updates whose dependency
closure includes a disabled mod are refused the same way.

`POST /instances/{id}/mods/updates/resolve` takes no body and returns `targets` (each
mod with a newer version in its own registry), `nodes` (every package that would
change, with `from_version` empty for a new dependency), and `backup`. To apply,
send `{"targets": [...]}` with the targets from the preview to
`POST /instances/{id}/mods/updates`. The job backs up the world, then updates all
targets together, rolling all of them back if one fails.

### Edit settings and files

Omitted fields in a settings `PATCH` remain unchanged. Unknown JSON fields are
rejected. Some launch changes set `restart_required`; saving settings does not
mean the running game has adopted them.

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
to chosen destinations and send `alert_opened` and `alert_resolved`.

When a rule covers the same incident as one of the three events, the rule's destinations
receive only the rule's alert. A failed backup is covered by a `job_failed` rule, a new
build by an `update_available` rule, and an unexpected stop by a `crash_loop` or
`instance_error` rule when this stop opens that condition. If the rule's condition is
already open, the rule sends nothing new, so its destinations still receive the event.
Destinations no rule names still receive every event.

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

| Topic                   | Payload                  |
| ----------------------- | ------------------------ |
| `instance.{id}.console` | Game log lines.          |
| `instance.{id}.stats`   | Resource samples.        |
| `instance.{id}.state`   | Server state changes.    |
| `job.{id}`              | Job progress and status. |

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
