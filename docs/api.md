# HTTP API and WebSockets

[Documentation](README.md) / API

The panel uses a JSON API under `/api/v1`. Requests use the same HTTPS origin as
the browser UI. This guide covers the implemented authentication flow, common
server operations, and live subscriptions.

Authentication uses session cookies. API keys and bearer-token authentication are
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

| Method  | Path                                     | Result or purpose                                                     |
| ------- | ---------------------------------------- | --------------------------------------------------------------------- |
| `GET`   | `/auth/me`                               | Current account.                                                      |
| `POST`  | `/auth/logout`                           | Revoke the session and clear its cookies.                             |
| `GET`   | `/me/permissions`                        | Current account's permissions.                                        |
| `GET`   | `/game/options`                          | Supported launch options and validation limits.                       |
| `GET`   | `/instances`                             | Visible servers.                                                      |
| `POST`  | `/instances`                             | Provision a server; returns a job.                                    |
| `GET`   | `/instances/{id}`                        | Server settings and current state.                                    |
| `PATCH` | `/instances/{id}`                        | Update supplied settings.                                             |
| `GET`   | `/instances/{id}/capabilities`           | Available server capabilities.                                        |
| `POST`  | `/instances/{id}/start`                  | Start a server; returns a job.                                        |
| `POST`  | `/instances/{id}/stop`                   | Stop a server gracefully; returns a job.                              |
| `POST`  | `/instances/{id}/restart`                | Restart a server; returns a job.                                      |
| `GET`   | `/instances/{id}/update-status`          | Game update availability.                                             |
| `POST`  | `/instances/{id}/update`                 | Update the game; returns a job.                                       |
| `GET`   | `/instances/{id}/logs`                   | Recent game logs.                                                     |
| `GET`   | `/instances/{id}/stats`                  | Current resource sample. Unknown values can be null.                  |
| `GET`   | `/instances/{id}/jobs`                   | Server job history.                                                   |
| `GET`   | `/instances/{id}/backups`                | World backup catalog.                                                 |
| `POST`  | `/instances/{id}/backups?mode=quiesced`  | Stop, back up, and resume a previously running server; returns a job. |
| `POST`  | `/instances/{id}/backups?mode=hot`       | Best-effort backup without stopping; returns a job.                   |
| `GET`   | `/instances/{id}/backups/{bid}/download` | Download an archive.                                                  |
| `POST`  | `/instances/{id}/backups/{bid}/restore`  | Restore into a stopped server; returns a job.                         |
| `GET`   | `/instances/{id}/worlds`                 | Worlds in the server's save directory.                                |
| `POST`  | `/instances/{id}/worlds/{name}/restore`  | Load another world already on disk; returns a job.                    |
| `DELETE` | `/instances/{id}/worlds/{name}`         | Delete a world from a stopped server; returns a job.                  |
| `GET`   | `/instances/{id}/mods`                   | Installed mods.                                                       |
| `GET`   | `/instances/{id}/configs`                | Available configuration files.                                        |
| `GET`   | `/instances/{id}/configs/{file}/raw`     | Raw configuration with an `ETag` header.                              |
| `PUT`   | `/instances/{id}/configs/{file}/raw`     | Replace raw configuration on a stopped server; requires `If-Match`.   |
| `GET`   | `/jobs/{id}`                             | Job status and result.                                                |
| `POST`  | `/jobs/{id}/cancel`                      | Request cancellation.                                                 |

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
