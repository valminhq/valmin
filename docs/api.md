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

A successful login returns the user object and sets three cookies:

| Cookie           | Purpose                                                                                 |
| ---------------- | --------------------------------------------------------------------------------------- |
| `valmin_session` | Authenticates the account. Secure and HttpOnly.                                         |
| `valmin_csrf`    | Supplies the token for authenticated writes. Secure and readable by browser JavaScript. |
| `valmin_device`  | Marks this client as trusted for the account's login limit. Secure and HttpOnly.        |

With [reverse proxy sign-in](configuration.md#sign-in-through-a-reverse-proxy) turned on,
a request from a trusted proxy that carries the username header is signed in without
`/auth/login`. Only a `GET` or `HEAD` opens a session from the header; its response sets
`valmin_session` and `valmin_csrf` for that member account. If the header names a different
user than the session cookie, the header wins: a read opens a session for the named user,
or is handled as signed out when that user is unknown, disabled or an administrator, and a
state-changing request is handled as signed out. A header from any other address is ignored. Administrator
accounts always sign in with `/auth/login`.

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

## Create the first administrator

Until an administrator exists, the API answers `503 setup_required`. The daemon logs a
setup token at startup; it expires after 15 minutes. Send it with the new account's
credentials to `POST /api/v1/setup`:

```json
{
  "token": "TOKEN_FROM_THE_LOG",
  "username": "admin",
  "password": "at-least-eight-characters"
}
```

Success creates the first administrator, who is also the panel's owner, signs it in,
and returns the account with the same two cookies a login sets. A wrong or expired
token is `422` on `token`. Once an administrator exists, the route answers
`410 setup_consumed`. It accepts five attempts a minute per client address.

`POST /api/v1/auth/login` accepts ten attempts a minute per client address and five per
username from one address. Failed logins for one username are also limited to thirty a
minute from all addresses together, except from a browser where that account signed in
before: a successful login sets a `valmin_device` cookie, scoped to the login route, that
exempts it; that browser's failures are capped at ten a minute instead. The cookie expires
after 180 days, and changing or resetting the password withdraws every such cookie. An IPv6 client address counts as its whole /64. Wrong credentials, and a disabled account, answer `401 invalid_credentials`.

## Schedules

Schedules run jobs on a cron timetable. Paths are relative to `/api/v1`.

| Method   | Path                  | Purpose                                                         |
| -------- | --------------------- | --------------------------------------------------------------- |
| `GET`    | `/schedules`          | Schedules you can see, with their next runs.                    |
| `POST`   | `/schedules`          | Create a schedule; returns `201` and the schedule.              |
| `PATCH`  | `/schedules/{id}`     | Change `cron`, `timezone`, `enabled`, `steps`, `apply` or player policy. |
| `DELETE` | `/schedules/{id}`     | Delete a schedule; returns `204`.                               |
| `POST`   | `/schedules/{id}/run` | Run a server schedule now; returns `202` and the job.           |

A create body has `kind`, `cron` (five fields), and `instance_id` for a kind that runs
against one server. `timezone` and `enabled` are optional; `enabled` defaults to `true`.
`kind` and `instance_id` cannot change after creation. Writing a schedule needs the
permission its kind needs:

| `kind`         | Runs against | Permission         |
| -------------- | ------------ | ------------------ |
| `backup`       | One server   | `backups.create`   |
| `restart`      | One server   | `instance.restart` |
| `game_update`  | One server   | `instance.update`  |
| `update_test`  | One server   | `instance.update`  |
| `chain`        | One server   | `instance.stop`, and each step's own |
| `update_check` | The panel    | `schedules.global` |
| `prune`        | The panel    | `schedules.global` |
| `alert_scan`   | The panel    | `schedules.global` |

An unknown kind is `422` on `kind`, and a server kind without `instance_id` is `422`
too. An `instance_id` that names no server you can see, or no server at all, is `404`. The list shows panel schedules to accounts with `schedules.global`, and a server's
schedules to anyone who can see that server. Each schedule also returns `last_run_at`,
`next_run_at` and `created_by_username`. A scheduled `game_update` on a modded server is
always skipped, because the update needs a person to confirm it; `update_test` is the way to
update one unattended.

### Test updates on a schedule

An `update_test` schedule runs the [update test](#test-updates-on-a-copy) on its server. Each
run tries the game when the last update check did not find the server current, and the mods
when any update is pending; with neither, the run is skipped. A run on a server that still
has a copy is skipped too. `apply` (boolean, default `false`) applies the tested updates once
the test ends, as `POST /instances/{id}/update-test/apply` would, but only when the copy's
mod loader finished, named every enabled plugin package without a failure and kept its count,
and the server is stopped or running with a player count of zero, read again when the test
ends. Otherwise the copy is kept. The `update_apply` job carries the schedule's id and the
test's `requested_by`.
Each schedule returns `apply`, `null` unless its kind is `update_test`; `apply` on any other
kind is `422` on `apply`. Like a `game_update`, an `update_test` schedule may run at most once
an hour.

### Run a chain of steps

A `chain` schedule carries `steps`, 1 to 20 of them, which run in order as one `chain` job.
Each schedule returns `steps`, `null` unless its kind is `chain`. `PATCH` replaces the whole
list. `steps` on any other kind, or a chain without it, is `422` on `steps`.

| `type`        | Field                  | Does                                                                                                  | Needs              |
| ------------- | ---------------------- | ----------------------------------------------------------------------------------------------------- | ------------------ |
| `command`     | `command`              | Sends one console command over RCON.                                                                  | `commands.send`    |
| `wait`        | `seconds`              | Waits 1 to 3600 seconds.                                                                              |                    |
| `wait_for`    | `condition`, `seconds` | Waits until the server is ready (`ready`) or has no players (`empty`), for at most 1 to 3600 seconds. |                    |
| `backup`      |                        | Takes a quiesced backup.                                                                              | `backups.create`   |
| `stop`        |                        | Stops the server.                                                                                     | `instance.stop`    |
| `start`       |                        | Starts the server.                                                                                    | `instance.start`   |
| `restart`     |                        | Restarts the server.                                                                                  | `instance.restart` |
| `game_update` |                        | Updates the game.                                                                                     | `instance.update`  |

Writing, editing or deleting a chain needs `instance.stop` and every action its steps need, so
`403` means the caller could not run a step by hand. A `command` outside the operator allowlist also needs
`commands.unrestricted`. A bad step is `422` on `steps[N].type`, `steps[N].seconds`,
`steps[N].condition` or `steps[N].command`. A chain with a step that stops the server may run at most once an hour,
and takes the player policy of `restart` and `backup` schedules.

Each lifecycle step is submitted as the job it normally is, with the schedule's id, and
waited for. The chain stops at the first step that fails and the `chain` job fails with the
step's number and reason, keeping the failed job's `error_code` when it has one. A `stop`
on a stopped server and a `start` on a running one are done. A `command` is skipped when the
server is not running or has no `Tristan-ValheimRcon`, and is sent again for up to 30 seconds
while the server's RCON listener is coming up. A `wait_for` ends as soon as its condition holds.
When `seconds` pass first, the job log records a warning and the chain goes on. `ready` waits
for the server's ready line, so it covers a `start` whose own wait ended before the world
loaded. `empty` waits for the server's own player count to read zero and keeps waiting while
the count is unknown. A `wait_for` is skipped when the server is not running, and fails the
chain if the server stops while it waits. A `game_update` step on a modded
server is refused when the run is submitted, so the run is recorded as skipped. A chain
is not resumed after the panel restarts, and one that fires while another chain on the
server is running is skipped. Cancelling the `chain` job stops it after the step in flight.

### Run a schedule now

`POST /api/v1/schedules/{id}/run` submits what a server schedule runs, at once, as its clock
would, and returns `202` with the job and its `Location`. The job carries the schedule, so
`scheduled` is `true`, and also names the caller in `requested_by_name`. Its steps behave as
on a scheduled run: a `stop` makes a backup when the server backs up on restarts and
automatic stops, and a backup is recorded as scheduled. The run ignores the waiting rules and
a planned power cut, and leaves `next_run_at`, `last_run_at` and a paused schedule as they
are. It needs what editing the schedule needs, and is entered in the audit log as
`schedules.run`.

A schedule holding a run for players, a panel schedule (`update_check`, `prune`,
`alert_scan`), a server in a state the kind cannot run from, and a `game_update` (or a chain
with one) on a modded server are `409 invalid_state`. So is an `update_test` with nothing
pending, or on a server that already has a copy, answered as `POST /instances/{id}/update-test`
answers it. A `backup`, `restart` or `game_update`
schedule on a server busy with another job, and a chain while another chain runs on the
server, are `409 job_in_progress`. Otherwise a chain is accepted and, as on its time, fails at
the first step the server cannot take.

### Read the scheduler timezone

`GET /api/v1/schedules` returns the collection fields `items`, `next_cursor`, and
`total`, plus a top-level `timezone`. The top-level timezone is `UTC`, including when `items`
is empty. This is the default for API clients that omit a schedule timezone.
Individual schedule records retain their `timezone` field. Send an IANA `timezone`
on create or patch to evaluate the cron expression in that zone; omitted values on
create default to UTC. Existing schedules remain UTC. A `cron` value with a `TZ=`
or `CRON_TZ=` prefix is refused with `422` so the zone has one source of truth.

### Read upcoming scheduled runs

Every schedule returns `upcoming_runs`, the next 5 times it fires as RFC 3339 UTC
timestamps, earliest first. The list starts at `next_run_at` while that time is still
ahead. A held run keeps its past `next_run_at` until it starts, so the list skips it. A
disabled schedule returns an empty list.

### Hold scheduled runs for players

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

| Field               | Meaning                                                                                                                                                   |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `requested_by_name` | Username of the person who started the job, or who ran a schedule now. Omitted for system work, runs the schedule clock started, and users since deleted. |
| `scheduled`         | `true` when a schedule started the job, run now included. Always present, and still `true` after the schedule is deleted.                                 |
| `changes`           | Mod jobs only, omitted for every other kind. The packages the job was asked to change, as `action` and `full_name` entries.                               |

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

While the data root has less free space than `data.free_space_floor_bytes`, jobs that
write a lot to it (provision, clone, world import, mod install, game update, backup,
restore, world tool, setup save, restore and rollback, update test and its apply) return
`409 insufficient_disk` with
`free_bytes` and `floor_bytes`. Other jobs still run.

Jobs that archive or replace world data (import, world restore and delete, backup
restore, clone, and game or mod updates) also ask Docker whether the game container
is running. If it was started outside the panel, the job fails with `error_code`
`instance_must_be_stopped` and leaves the world unchanged.

`POST /api/v1/jobs/{job_id}/cancel` requests cancellation and returns 204. Cancellation
is cooperative and can be refused with `409 job_not_cancellable` after a job
passes its cancellation point.

`GET /api/v1/instances/{id}/jobs` lists a server's jobs, newest first, with the usual
`cursor` and `limit` pagination. It needs `instance.view` and nothing more. Add
`scheduled=true` to list only the runs a schedule started. `since` (inclusive) and `until`
(exclusive), as RFC 3339 timestamps, keep the jobs that finished in that window; a job that
has not finished counts by when it was created. A `scheduled` that is not a boolean, or a
timestamp that does not parse, returns `400 invalid_parameter`. A skipped scheduled run, such as one that found another
job holding the server, is recorded as `cancelled` with an `error_code` and an `error`
that explains why. A job an operator cancelled has no `error_code`.

## Player count history

`GET /api/v1/instances/{id}/players/history` requires `instance.view` and `stats.read`.
Without range parameters, it returns the existing newest-first cursor page. Add both
`from` and `to` as RFC3339 timestamps to request changes in `[from, to)`. The requested
duration must be at most 30 days. A future `to` is clamped to server time. Invalid or
unpaired bounds return `400 invalid_parameter`.

Ranged pages retain `items`, `next_cursor`, and `total`, and add `range` with the
effective `from`, effective `to`, and `initial`. `initial` is the latest retained
observation before `from`, or `null` if none exists. Use the effective bounds and the
returned cursor on every subsequent page. Each observation's `players` may be `null`:
that means the panel could not determine the count, not that it observed zero. History
is pruned after 30 days.

## Common endpoints

Paths in this table are relative to `/api/v1`. Each operation checks the account's
permissions as well as the server's current state. IDs in braces are path parameters.

| Method   | Path                                      | Result or purpose                                                     |
| -------- | ----------------------------------------- | --------------------------------------------------------------------- |
| `POST`   | `/setup`                                  | Create the first administrator; see above.                            |
| `POST`   | `/auth/login`                             | Sign in; sets the session and CSRF cookies.                           |
| `GET`    | `/auth/me`                                | Current account.                                                      |
| `POST`   | `/auth/logout`                            | Revoke the session and clear its cookies; `204`, or `200` with `redirect_url` when a proxy logout URL is set. |
| `GET`    | `/me/permissions`                         | Current account's permissions.                                        |
| `POST`   | `/me/password`                            | Change your own password; returns `204`.                              |
| `GET`    | `/me/sessions`                            | Your signed-in devices; see below.                                    |
| `DELETE` | `/me/sessions/{id}`                       | Sign one of your own devices out; returns `204`.                      |
| `PATCH`  | `/me`                                     | Change your own preferences; returns the account.                     |
| `GET`    | `/game/options`                           | Launch options and validation limits; any signed-in account.          |
| `GET`    | `/game/patch-notes`                       | The game's recent patch posts from Steam; any signed-in account.      |
| `GET`    | `/instances`                              | Visible servers.                                                      |
| `GET`    | `/instances/inbox`                        | Open alert conditions on visible servers and the host.                |
| `POST`   | `/instances`                              | Provision a server; returns a job.                                    |
| `GET`    | `/instances/{id}`                         | Server settings and current state.                                    |
| `PATCH`  | `/instances/{id}`                         | Update supplied settings.                                             |
| `DELETE` | `/instances/{id}`                         | Delete a stopped server; returns a job.                               |
| `POST`   | `/instances/{id}/clone`                   | Copy a stopped server; returns a job.                                 |
| `GET`    | `/instances/{id}/capabilities`            | Available server capabilities.                                        |
| `POST`   | `/instances/{id}/commands`                | Send one console command over RCON; returns the reply.                |
| `POST`   | `/instances/{id}/world-tools`             | Back up, start and run a world-maintenance mod's command; returns a job. |
| `GET`    | `/instances/{id}/password`                | Game password. Needs `instance.view`; every read is audited.          |
| `GET`    | `/instances/{id}/reachability`            | Whether players outside the host can find the server; needs `instance.view`. |
| `POST`   | `/instances/{id}/start`                   | Start a server; returns a job.                                        |
| `POST`   | `/instances/{id}/stop`                    | Stop a server gracefully; returns a job.                              |
| `POST`   | `/instances/{id}/restart`                 | Restart a server; returns a job.                                      |
| `POST`   | `/instances/{id}/acknowledge`             | Re-check a server parked in `error`; needs start permission, audited. |
| `GET`    | `/instances/{id}/update-status`           | Game update availability against the server's own branch.             |
| `POST`   | `/instances/{id}/update`                  | Update the game; returns a job.                                       |
| `GET`    | `/instances/{id}/update-test`             | The server's update-test copy and how it booted, or `{"test": null}`. |
| `POST`   | `/instances/{id}/update-test`             | Copy the server and try updates on the copy; returns a job.           |
| `POST`   | `/instances/{id}/update-test/apply`       | Install the tested updates on the server; returns a job.              |
| `GET`    | `/instances/{id}/logs`                    | Recent game logs.                                                     |
| `GET`    | `/instances/{id}/diagnosis`               | Why the current start failed or is degraded; needs `console.read`.    |
| `GET`    | `/instances/{id}/stats`                   | Current resource sample. Unknown values can be null; see [stats samples](#stats-samples). |
| `GET`    | `/instances/{id}/disk`                    | Disk use by category and free space; needs `stats.read`.              |
| `GET`    | `/instances/{id}/operation`               | The unfinished create or import operation, or `null`.                 |
| `POST`   | `/instances/{id}/operation/resume`        | Continue an interrupted operation; returns a job.                     |
| `POST`   | `/instances/{id}/operation/abandon`       | Drop the steps that never ran; returns the operation.                 |
| `GET`    | `/instances/{id}/jobs`                    | Server job history; `scheduled=true` lists scheduled runs only, `since`/`until` bound it. |
| `GET`    | `/instances/{id}/backups`                 | World backup catalog, with each archive's `world_bytes` and `world_attention`, and `removed_by_next_consistent` and `removed_by_next_best_effort`: the `{id, created_at}` of the archives retention deletes once the next backup of that kind is taken. |
| `POST`   | `/instances/{id}/backups?mode=quiesced`   | Stop, back up, and resume a previously running server; returns a job. |
| `POST`   | `/instances/{id}/backups?mode=hot`        | Best-effort backup without stopping; returns a job.                   |
| `GET`    | `/instances/{id}/backups/{bid}/download`  | Download an archive.                                                  |
| `GET`    | `/backups/orphaned`                       | World archives left by deleted servers; administrators only.          |
| `DELETE` | `/instances/{id}/backups/{bid}`           | Delete an unlinked archive.                                           |
| `POST`   | `/instances/{id}/backups/{bid}/restore`   | Restore into a stopped server; returns a job.                         |
| `POST`   | `/instances/{id}/backups/{bid}/acknowledge` | Clear the archive's world check findings; needs `backups.restore`.  |
| `GET`    | `/instances/{id}/setups`                  | Saved setups for the server.                                          |
| `POST`   | `/instances/{id}/setups`                  | Save a setup from a stopped or running server; returns a job.         |
| `GET`    | `/instances/{id}/setups/{sid}`            | One saved setup and its linked world backup.                          |
| `GET`    | `/instances/{id}/setups/{sid}/configs/{file}` | The saved text of one config file of the setup.                   |
| `GET`    | `/instances/{id}/setups/{sid}/preview`    | Compare and validate a restore; returns an `etag`.                    |
| `POST`   | `/instances/{id}/setups/{sid}/restore`    | Restore a stopped server; requires `If-Match`, returns a job.         |
| `POST`   | `/instances/{id}/setups/{sid}/rollback`   | Roll back a stopped or running server; returns a job.                 |
| `DELETE` | `/instances/{id}/setups/{sid}`            | Delete a setup and release its links; returns a job.                  |
| `GET`    | `/instances/{id}/worlds`                  | Worlds in the server's save directory.                                |
| `POST`   | `/instances/{id}/worlds/import`           | Upload a world into a stopped server; returns a job.                  |
| `POST`   | `/instances/{id}/worlds/import-backup`    | Import a world from another server's backup or a deleted server's archive; returns a job. |
| `POST`   | `/instances/{id}/worlds/{name}/restore`   | Load another world already on disk; returns a job.                    |
| `DELETE` | `/instances/{id}/worlds/{name}`           | Delete a world from a stopped server; returns a job.                  |
| `GET`    | `/instances/{id}/players/history`         | Player count history; see above.                                      |
| `GET`    | `/instances/{id}/players/seen`            | Accounts the server named in its log, with their names.               |
| `GET`    | `/players`                                | Every account any server has seen; administrators only.               |
| `GET`    | `/instances/{id}/admins`                  | Admin list, with an `ETag`. `bans` and `permitted` work the same.     |
| `PUT`    | `/instances/{id}/admins`                  | Replace the admin list; requires `If-Match`.                          |
| `POST`   | `/mods/sync`                              | Refresh every enabled registry now; returns a job.                    |
| `GET`    | `/mods/search`                            | Search the cached catalogue across registries.                        |
| `GET`    | `/mods/{namespace}/{name}`                | One catalogue package and its version history.                        |
| `GET`    | `/instances/{id}/mods`                    | Installed mods.                                                       |
| `POST`   | `/instances/{id}/mods/resolve`            | Preview the dependency closure an install would apply.                |
| `POST`   | `/instances/{id}/mods`                    | Install a mod and its dependencies; returns a job.                    |
| `DELETE` | `/instances/{id}/mods/{full_name}`        | Uninstall a mod; returns a job.                                       |
| `PATCH`  | `/instances/{id}/mods/{full_name}`        | Change a mod's client tag or lock, or enable or disable it.           |
| `GET`    | `/instances/{id}/mods/{full_name}/changelog` | The `CHANGELOG.md` a package version ships; `version` is required. |
| `POST`   | `/instances/{id}/mods/code/preview`       | Preview applying a r2modman or Gale profile code.                     |
| `POST`   | `/instances/{id}/mods/code/apply`         | Back up the world, then apply those changes; returns a job.           |
| `POST`   | `/instances/{id}/mods/updates/resolve`    | Preview updating every mod that has a newer version.                  |
| `POST`   | `/instances/{id}/mods/updates`            | Back up the world, then apply those updates; returns a job.           |
| `GET`    | `/instances/{id}/mods/export`             | Client manifest preview, or the archive with `format=r2z`.            |
| `GET`    | `/instances/{id}/mods/export/code`        | The stored profile code while the mods still match it.                |
| `POST`   | `/instances/{id}/mods/export/code`        | Upload the client manifest; returns a profile code. `409` if empty.   |
| `GET`    | `/instances/{id}/mods/queue`              | Installs waiting for the server to stop.                              |
| `POST`   | `/instances/{id}/mods/queue`              | Queue an install for the next stop or restart.                        |
| `POST`   | `/instances/{id}/mods/updates/queue`      | Queue confirmed updates together, all or none; returns the queue.     |
| `DELETE` | `/instances/{id}/mods/queue/{full_name}`  | Remove a queued install.                                              |
| `GET`    | `/instances/{id}/manifest`                | Server definition: settings, pinned mods, and config files.           |
| `GET`    | `/instances/{id}/manifest/code`           | The server as a template code.                                        |
| `GET`    | `/instances/{id}/manifest/files`          | The config files templates carry, and the changed ones they leave out. |
| `POST`   | `/instances/manifest/preview`             | Check a server definition before importing it.                        |
| `POST`   | `/instances/import`                       | Create a server from a definition; returns a job.                     |
| `GET`    | `/templates`                              | Shared templates from the Valmin presets repository.                  |
| `GET`    | `/instances/{id}/configs`                 | Available configuration files.                                        |
| `GET`    | `/instances/{id}/configs/{file}`          | Settings of one `.cfg` file, typed for a form, with an `ETag`.        |
| `PATCH`  | `/instances/{id}/configs/{file}`          | Change a `.cfg` file's settings by key; returns the updated settings. |
| `GET`    | `/instances/{id}/configs/{file}/original` | Settings of the file's baseline version (`.cfg`).                     |
| `GET`    | `/instances/{id}/configs/{file}/previous` | Settings of the newest kept version that differs from the file (`.cfg`). |
| `GET`    | `/instances/{id}/configs/{file}/history`  | The file's kept versions, newest first.                               |
| `GET`    | `/instances/{id}/configs/{file}/history/{rev}` | Settings of one kept version (`.cfg`).                           |
| `GET`    | `/instances/{id}/configs/{file}/history/{rev}/raw` | One kept version's bytes.                                    |
| `GET`    | `/instances/{id}/configs/{file}/raw`      | Raw configuration with an `ETag` header.                              |
| `PUT`    | `/instances/{id}/configs/{file}/raw`      | Replace raw configuration on a stopped or running server; `If-Match`. |
| `DELETE` | `/instances/{id}/configs/{file}`          | Delete a configuration file; its history is kept.                     |
| `GET`    | `/instances/orphans`                      | Containers Valmin created that no server claims.                      |
| `GET`    | `/orphans/{container_id}`                 | What the panel can prove about one unclaimed container.               |
| `POST`   | `/orphans/{container_id}`                 | Recover the container as a server; returns a job.                     |
| `DELETE` | `/orphans/{container_id}`                 | Remove an unclaimed container; its files stay on disk.                |
| `GET`    | `/jobs/{id}`                              | Job status and result.                                                |
| `POST`   | `/jobs/{id}/cancel`                       | Request cancellation.                                                 |

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

Optional fields include `preset`, `modifiers`, `cpu_limit`, `mods`, `seed`, `game_branch` and
`game_branch_password`.
`game_branch` names the Steam branch to install, `public` when omitted. Without
`game_branch_password` it must be `public` or a branch in the `branches` list of
`/game/options`, which holds the branches the last update check saw. A password branch, which
Steam never lists, needs `game_branch_password`; `public` refuses one. Either field answers
`422` when it is not 1 to 64 printable characters, or starts with `+` or `-`.
`modifiers` maps a world modifier to its value, such as `{"combat": "hard"}`. A world key
such as `nomap` takes the value `"on"`, and any other value answers `422`. Leave a
modifier out to use the default. The `modifiers` and `world_keys` fields of
`/game/options` list the keys, values and labels.
Each mod selection contains `full_name` and `version`. `seed` is written to the custom seed
mod's config before the first start. It takes 1 to 10 letters or digits and needs the
package named by `custom_seed` in `/game/options` among `mods`, either listed itself or as
a direct dependency of a listed package; otherwise the request answers `422` on `seed`.
Creation is administrator-only.
See [Create a server](usage.md#create-a-server) for the creation flow.

`PATCH /api/v1/instances/{id}` changes only the fields it is sent; `world_name` cannot be
changed. A new `name` must not be blank or used by another server, which is
`409 name_taken`. Renaming needs no restart.

### Delete, clone and recover servers

These routes are for administrators only.

`DELETE /api/v1/instances/{id}` deletes a server in the `stopped` or `error` state and
returns a job. It removes the container, the game installation with its mods and
configuration, the logs and the server's record. The world files and backup archives
stay in the data root unless the request adds `keep_worlds=false`, which removes them
too.

`POST /api/v1/instances/{id}/clone` with `{"name": "Copy of Friends"}` copies a stopped
server: world, game build, mods, configuration files, launch settings and password. The
copy gets its own ports and identity and is left stopped. Users, grants and backups are
not copied. A source that is not stopped answers `409 invalid_state`.

### Test updates on a copy

Starting a test needs clone, update, mod management and start on the server, and applying
it also needs stop and delete, which only administrators hold. The routes apply to a server
in the `stopped` or `running` state.

`POST /api/v1/instances/{id}/update-test` with `{"game": true, "mods": true}` returns a
`202` `update_test` job. At least one of the two must be true, or the request answers
`422`. When neither chosen part has anything to try (the last check found the game current,
and no mod update is pending), it answers `409 invalid_state`. The job copies the server, puts the new game build (`game`) and the pending mod
updates (`mods`) on the copy, starts it, waits for its world to load and logs what the mod
loader said about each plugin.
A running source is copied live, so the copy's world is as of the game's last save and its
seed backup is not marked consistent. The copy is a server whose `test_of` is the source's
id. It is not public, has crossplay off, runs with the source's `mem_limit_mb` and half its
`cpu_limit` (half the host's cores when the source has none, and never below one core), and
has `auto_stop_minutes` of 10. The request answers:

- `409 invalid_state` when the server already has a copy, with `details.test_copy_id`, or is
  itself a copy;
- `409 insufficient_memory` with `available_mb` and `needed_mb` when the host's available
  memory is below the copy's memory limit;
- `409 insufficient_disk`, as for any job that writes a lot.

`GET /api/v1/instances/{id}/update-test` answers `{"test": null}` when the server has no
copy, and otherwise `{"test": {...}}` with `copy_id`, `copy_name`, `copy_state`, `copy_port`,
`copy_mem_limit_mb`, `copy_cpu_limit`, `branch`, the `source_build_id` and `copy_build_id`,
`game_newer` (the copy's build is newer, so an apply installs it), `applicable` (the copy is
stopped or running and its test ran to the end), `apply_blocker` (why it is not applicable, as
a sentence, or null), the
`mod_changes` (`full_name`, `from`, `to`, `source`) where the copy runs a newer version,
`failed` (`full_name`, `load_error`) and `not_seen` for the copy's load verification, and
`plugin_load`, which is null until the copy's loader has written a log. Its `complete` is
false while the loader has not logged that it finished, and the counts are then partial.
While the copy is being created the build ids are empty and the lists are empty. The copy has no grants of
its own, so an account that cannot see the copy, which is anyone but an administrator, gets
`{"test": null}` even when the server has one.

`POST /api/v1/instances/{id}/update-test/apply` returns a `202` `update_apply` job. It
stops the server if it runs, installs the copy's game build from the build cache if it is
newer than the server's, installs the copy's mod versions where they are newer than the
server's, starts the server again if it was running, and deletes the copy with its world
and backups. A failed step ends the job and leaves the copy; a server that was stopped for
it stays stopped. The job can be cancelled until it starts changing the server; after that
a cancel answers `409 job_not_cancellable`. A server without a copy answers `404`; a copy
that is not `applicable` answers `409 invalid_state`.

`GET /api/v1/game/patch-notes` answers `{"notes": [...]}`, at most five patch posts from
Steam's news for the game, newest first. Each has `title`, `url`, `published_at` and `text`
as plain lines. The panel keeps the answer for an hour and serves the previous one if Steam
cannot be reached; with none, the route answers `503 unavailable`.

`GET /api/v1/instances/{id}/mods/{full_name}/changelog?version=1.2.0` returns
`version`, `text`, `truncated` and `registry_url` for the root `CHANGELOG.md` of that package
version, read from the zip the install would use (an optional `source` names the
registry). It needs `mods.manage`, since it can download the package into the cache; the
route has no request timeout, so the first read of a large package waits for the download,
which ends only when it stalls. `text` is cut at 256 KiB. A version without the file, or one the registry's index
does not list, answers `404`; a registry that cannot be reached answers `503`.

`GET /api/v1/instances/orphans` lists containers labelled as Valmin's whose server record
is missing, each with `container_id`, `name`, `instance_id`, `base_port` and `running`.
`GET /api/v1/orphans/{container_id}` adds what the panel can read from the container:
`crossplay_instance_id`, `game_build_id`, `modded`, and `required_fields`, the fields a
recovery request must send. `POST /api/v1/orphans/{container_id}` takes `name`,
`server_name`, `world_name`, `password`, `public`, `crossplay`, `preset`, `modifiers`,
`extra_args`, `mem_limit_mb` and `cpu_limit`. They must describe the container exactly:
a mismatch answers `409 container_mismatch`, a taken name `409 name_taken`, and a
container already claimed `409 invalid_state`. Recovery never stops, recreates or
changes the container. A container whose image was removed, or whose data directory is gone,
cannot be recovered: the preview and the recovery answer `409 container_mismatch` with
`details.reason` `image_missing` or `data_missing`. `DELETE /api/v1/orphans/{container_id}`
removes a stopped unclaimed container and answers `204`; it leaves the data directory alone, is
audited as `orphans.remove`, answers `409 invalid_state` while the container runs, and `404` for a
container that is not an unclaimed Valmin container.
These four routes answer `403` without the permission.

### Interrupted operations

Creating a server, and creating one from a definition, run as a chain of jobs:
provisioning, then each mod, then the configuration. `GET /api/v1/instances/{id}/operation`
returns the chain that has not finished, or `null`. It has `id`, `kind` (`create` or
`import`), `state` (`running`, `interrupted`, `completed` or `abandoned`), `cursor`, the
index of the next step, and `steps`, each with `kind` and the `job_id` that ran it.

When the daemon stops mid-chain, the operation becomes `interrupted` and waits.
`POST .../operation/resume` submits the next step and returns its job.
`POST .../operation/abandon` drops the steps that never ran and keeps what the finished
steps built. Both need `instance.create`, and both answer `409 invalid_state` for an
operation in another state.

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
    {
      "source": "thunderstore",
      "enabled": true,
      "synced_at": "2026-09-21T12:00:00Z"
    },
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

`POST /mods/sync` refreshes every enabled registry without waiting for the next
interval. It needs `panel.settings`, so a member gets `404`. It returns `202` and a
job; while a refresh is already running it answers `409 job_in_progress` with that
job's `job_id`. A registry that reports its index unchanged still counts as
refreshed, so `synced_at` moves either way.

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
`setup` is `true` when the install changes anything and the server's `setup_before_update` is
not `off`: the job then also saves the current setup first, as an automatic setup linked to
that backup when it takes one.

An install that would overwrite files no installed mod owns, such as files placed by hand,
writes nothing and fails with `mod_unowned_files`; its `error` names the files. Send the
install again with `"unowned": "keep"` to install the mod without those files, which stay
as they are and are never removed with the mod, or `"unowned": "replace"` to overwrite them
and make them the mod's own, removed with it. The choice does not cover config files under
`BepInEx/config`: an install keeps one that exists, and an update replaces one only when the
mod ships a changed version of it. `POST /instances/{id}/mods/updates` takes the same `unowned` field.
Queued installs and installs from a server definition send none, so they stop at such files
until you install by hand.

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
`load_status` of `loaded`, `not_seen`, `failed`, `pending`, or `null`. `pending` is a plugin
installed after the server's last mod loader run, which could not have loaded it. When the status is `failed`,
`load_error` carries the mod loader's own message. `not_indexed` is `true` for a mod its
registry no longer lists: the last complete refresh of that registry's catalogue did not
include it. Such a row has an empty `update_version` and is left out of **Update all**.
It stays `false` until the registry has completed at least one refresh. `file_count` counts
every file the mod placed, plus the config files it ships that were already on the server or
that another mod placed, and `config_file_count` those under `BepInEx/config/`. By
default, uninstall keeps the config files, including edited settings. `leftover_configs` names the `.cfg` files
in `BepInEx/config/` that belong to this mod alone: the ones it placed, and the ones its
plugin wrote, matched by the mod's name, its plugin `.dll` names and each file's header.
`DELETE /instances/{id}/mods/{full_name}?remove_configs=true` removes those files too, and
those of any dependency removed with `remove_orphans=true`, along with the copies the panel
kept of them. It needs `config.edit` as well as `mods.manage`, and a failed uninstall puts
them back. `locked` is `true` for a locked mod and
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
change, with `from_version` empty for a new dependency), `conflicts`, `backup` and `setup`.
`setup` is `true` unless the server's `setup_before_update` is `off`. Locked mods, modpacks, and members that follow their modpack are never targets; update
a modpack by installing its new version. To apply, send `{"targets": [...]}` with the
targets from the preview to `POST /instances/{id}/mods/updates`. The job backs up the
world and, when `setup` is `true`, saves the current setup, then updates all targets together, rolling all of them back if one fails. It is
refused while the preview has conflicts.

`POST /instances/{id}/mods/code/preview` takes `{"code": "<profile code>", "select": [...]}`
and applies a r2modman or Gale profile code to this server as a dry run. It fetches the code
as the import routes do. `select` names exactly the packages to apply; left out, installs and
raised versions are chosen and lowered versions are not. The answer has `profile_name`,
`registry` (the one that served the code), `matching` (packages already at the code's
version), `rows`, `targets`, `nodes`, `conflicts`, `problems`, `backup` and `setup`, which
mean what they do for the update preview. Each row has
`full_name`, `installed_version` (empty when not installed), `version`, `source`, `selected`,
and either `change` (`install`, `upgrade` or `downgrade`) or `skip`, one of `locked`,
`disabled`, `modpack`, `framework`, `other_registry`, `not_in_registry`, `registry_disabled`,
`unknown_registry` or `unavailable`. `problems` and `conflicts` stop the apply.

An installed mod never changes registry. A code that names another registry for it, or whose
version its registry lacks, leaves it out with the reason; it does not use the other registry.
A mod that is not installed comes from the registry the code names, else the one that served
the code if it has that exact version, else any enabled registry that has it. Dependencies the
code does not list prefer the registry that served the code. Mods that are not in the code are
never touched, and neither are locked, disabled and modpack-managed mods or the installed mod
framework (which is never installed from a code). The code's config files are not applied. A server
that has no mod framework yet is refused on apply and reported under `problems` on preview; install
any mod first.

To apply, send `{"targets": [...], "registry": "<registry>", "unowned": "keep"}` with the
`targets` and `registry` from the preview to `POST /instances/{id}/mods/code/apply`. It needs
a stopped server (`409` otherwise) and answers `202` with a job that backs up the world, then
installs, raises and lowers all targets together, rolling all of them back if one fails. A
target for an installed mod must name its installed registry and a different version, and one
code applies at most 1,000 packages.

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

A definition's `configs` are the config files the panel changed, each whole: files the panel
saved or a setup restore wrote, apart from the RCON plugin's file, whose bytes differ from the
version the panel first found or the newest mod install wrote. A file left as found is not
carried, since the mods create it again. `schema` is `1` when every file is a `.cfg` directly in
`BepInEx/config` and `2` when one has a path into a subfolder or is a YAML, JSON or text file;
`file` is then that path under `BepInEx/config`. The preview and import read both; a panel from
before schema 2 refuses a schema 2 file. A `.cfg` over 1 MiB is left out. The export answers
`422 validation_failed` when the changed files would be more than an import accepts: over 200
files, or a document over about 8 MiB. Importing a definition writes each file whole, creating
the folders its path needs; a schema 2 file whose path runs through another of its files is
refused.

`GET /instances/{id}/manifest/files` needs the same actions and returns
`{"carried": [{"file", "size_bytes"}], "left_out": [{"file", "reason"}]}`: the files a definition
carries, and the changed files templates leave out, such as one changed only outside the panel
or one over 1 MiB, with `reason` written for people.

`GET /instances/{id}/manifest/code` needs the same actions as the definition export and
returns `{"code", "mods", "settings", "secrets_left_out", "disabled_left_out", "configs"}`. The
code starts with `valmin1:` and carries enabled mods and the config settings changed in the
panel, in the `.cfg` files directly in `BepInEx/config`: its format holds plain `.cfg` names only,
so other config files go only in a definition. Each entry of `configs` is a file with changed
settings: `file`, `excluded`, `secrets_included`, and `settings` with `key`, `value`, `secret`
(the name looks like a credential) and `suspicious` (the value does). Secret settings are left
out of the code and counted in `secrets_left_out`; repeat `?include_secrets=<file>` to keep a
file's. Repeat `?exclude=<file>` to leave whole config files out; they stay listed with
`excluded` set.
Send it as `{"code": "valmin1:..."}` in place of `manifest` to the preview and import routes.
For a code, each preview entry in `configs` also lists `settings`, shaped like the export's.
Whitespace in the code is ignored. Dependencies the code leaves out are derived from the
catalogue, and its settings are merged into the config files the mods create. A definition
holds at most 1,000 mods.

A code that is a UUID, such as `11111111-2222-3333-4444-555555555555`, is a profile code from
r2modman or Gale. The panel asks Thunderstore for it, then Hexium when that registry is enabled,
and answers `422` on `code` when neither has it. When a registry is down and the other does not
have the code, the answer is `503`. The enabled mods of the profile become the definition's mods,
and a mod that names its `source` keeps it. The mods nothing else in the profile depends on come
first and install as the explicit choices; the rest follow as their dependencies. The profile's
`BepInEx/config` files are merged like a template code's settings, except `BepInEx.cfg`, which
the panel keeps so it can read the server's plugin lines. The files together may hold 8 MiB. The server and its definition
are named after the profile, with the world `Dedicated`. Profile codes use the registries'
`/api/experimental` endpoints, which can change without notice.

`POST /instances/{id}/mods/export/code` needs the same actions as `GET /instances/{id}/mods/export`
and takes no body. It uploads the same client mod list as the file export to a registry, stores
the result as the server's current profile code, and returns `{"code", "registry", "created_at"}`. The registry is `thunderstore` unless a mod in the list exists only
in Hexium and Hexium is enabled, in which case it is `hexium`, every mod in it names its registry,
and Gale reads the code but r2modman does not. A list with no mods answers `409 invalid_state`, and
this file export with `format=r2z` does the same. A list with a conflict answers `409 mod_conflict` and uploads nothing, a list
the registry refuses as too large answers `413`, and a registry that is down answers `503`. A user
may upload five codes a minute (`429` with `Retry-After` beyond that), counted only when the upload
is attempted. Each entry in the export preview also has `source`, the registry the installed package
came from.

`GET /instances/{id}/mods/export/code` needs the same actions and returns `{"current": {"code",
"registry", "created_at"}}`, or `{"current": null}` when no code was uploaded, the mods players need or
their versions or registries have changed since, or the list has a conflict.
It never uploads.

`GET /templates` needs the action that creates servers and returns
`{"sources", "templates", "problems"}`. Each source has `url`, `fetched_at` and `error`.
Each template has `source`, `slug`, `name`, `description`, `mods` and `code`. Send its `code`
to the preview and import routes. Each problem has `source`, `path` and `detail` for a
`template.yaml` that could not be used. Repositories are read again after an hour; add
`?refresh=true` to read them now.

### Saved setups

Saving, deleting and the `restore` route need `setups.manage`, which only administrators hold.
Listing, reading and previewing setups and `rollback` also accept a member with `mods.manage`.
Reading a setup's config text needs `config.raw`, as a config file's history does, and the
`content` of `.cfg` files in a setup or preview is left out without it. Send `{"name":"Working before update"}`
to `POST /instances/{id}/setups` while the server is stopped or running; any other state is
`409 invalid_state` with `allowed_states`. Add
`"world_backup_id":"BACKUP_ID"` to link a consistent backup from the same server, and
`"light":true` to keep only the packages whose installed files differ from their cached
registry archive or whose registry is disabled. `"back_up_world":true` takes a consistent
backup during the save, with trigger `manual`, verified and world-checked like any manual
backup, and links it. A backup that fails its check, or a data root below its free-space floor,
fails the save job and publishes nothing. It needs `backups.create` and cannot be combined with
`world_backup_id`, which is `422 validation_failed`. A server whose selected world has no
`.db` or `.fwl` yet gets no backup; a failed check carries the error code
`backup_unverifiable`, as a manual backup's does. `back_up_world` on a running server is `422`.
`"link_next_world_backup":true` saves the setup with `world_pending` set instead: the next
`pre_update` backup a mod update or setup restore takes is linked to it. Any other change to the
installed mods before then (a package added or removed, or its registry, version or enabled state
changed, even back again) clears `world_pending`, and the setup keeps no backup. It cannot be combined with `world_backup_id` or
`back_up_world` (`422`). A setup's `world_backup` includes the archive's `size_bytes`.
A save of a running server fails if a mod or config file changes while it is read.
The save job publishes the setup only after it captures every required package.
Names can repeat; use the setup ID to identify one.

A setup records launch and backup settings, exact versions and registries of
Valmin-managed mods, their locks, enabled states and client tags, and every file
`GET /instances/{id}/configs` lists under `BepInEx/config` except the RCON plugin's file.
Config bytes are stored once per content and shared between setups. It retains package
payloads for offline restore. Game build and world name are comparison-only. The server
password, selected world, and world files are not restored.

Each setup has `created_by`, `config_tree`, `configs_truncated`, `automatic`, `light`,
`world_pending` and `configs_digest`. `configs_digest` is a hash over the setup's config file
paths and bytes: two setups holding the same files have the same digest. The list carries each
setup's `mods` and `configs_digest`, but not its `configs`.
`created_by` is the username of whoever saved the setup or made the change an automatic one
comes before, `null` for the panel's own change or a deleted account. `config_tree` is
`false` for a setup saved before setups held the whole config tree: it holds only the `.cfg`
files directly in `BepInEx/config`, and restoring it changes only those. `configs_truncated` is
`true` when the listing limits left files out of the setup; a restore leaves such files alone.
`automatic` marks a setup the panel saved from the server just before it changed anything:
a restore's, named `Before restoring <name>`, or a mod change's, named
`Before installing <mods>`, `Before removing <mods>`, `Before updating <mods>` or
`Before changing mods` (see `setup_before_update`). It links the `pre_update` backup the job
took first, when it took one: a restore that changes mods, an update, and an install that backs
up the world first take one, while a plain install or a removal does not. The installs that
create a server save none. Each server keeps its newest `setup_versions_kept` automatic setups, and the newest
`setup_worlds_kept` of those that link a backup keep it. If that capture fails, the job goes
ahead and its log says so. A `light`
setup restores the packages it does not keep from the download cache or their registry; its
preview lists, in `problems`, each such package that is not cached and that no enabled
registry lists.

Each entry of `configs`, in a setup and in a preview's `current`, has `file` (the path under
`BepInEx/config`), `sha256` and `size_bytes`; `content` is included for a `.cfg` directly in
`BepInEx/config` only. Read any other saved file from
`GET /instances/{id}/setups/{sid}/configs/{file}`, with the path's slashes escaped as on the
config routes, and the current one from `GET /instances/{id}/configs/{file}/raw`.

`GET /instances/{id}/setups/{sid}/preview` compares the saved setup with the
current server and checks whether its packages, config files and settings can be applied.
For a setup whose `config_tree` is `false`, `current.configs` lists only the files that
restore would change. The response contains `etag`, `ready`, `problems`, `current`, and
`setup`, and `stop_required`, which is `true` while the server is not stopped: a restore
refuses it, and a rollback stops a running one first. Send that `etag` as `If-Match` to
`POST /instances/{id}/setups/{sid}/restore` while the server is stopped. Missing or stale
fingerprints return `412 stale_write`. A restore
removes config files the setup does not hold and keeps each file it replaces or removes in
that file's history. A failed installation rolls back its changes, and the restore never
starts the server.

`POST /instances/{id}/setups/{sid}/rollback` does the whole undo as one job (`setup_rollback`,
not cancellable): it stops the server if it runs, restores the setup, restores the world
backup the setup links with `{"world":true}`, and starts the server again with
`{"start":true}`. `{"mods_only":true}` restores mods and config files and keeps the current
launch and backup settings, which it then leaves as they are; a caller without
`setups.manage` always gets it. `GET …/preview?mods_only=true` checks such a rollback, leaving
the saved settings out of `problems`, as a preview for a caller without `setups.manage` always
does. It needs `setups.manage` or `mods.manage`, plus
`instance.stop` for a running server,
`instance.start` for `start` and `backups.restore` for `world`. A server that is neither stopped
nor running returns `409 invalid_state`; `world` without an available linked backup is `422`
naming `world`. On a stopped server an optional `If-Match` is checked against the preview's
`etag` (`412 stale_write` when it differs); on a running server it is ignored, and the job
fingerprints the server once it is stopped. A step whose job meets another job holding a lock
it needs waits for that job and tries again, up to three times. A step runs only after the one
before succeeded, so a failed setup restore never restores the world or starts the server, and
a stop that was done stays done. A failed world restore leaves the setup restored and the server
in `error`. Each step is its own job, so with `world` the current
world is archived by the setup restore when the mods change and once more as the world
restore's own `pre_restore` snapshot.

A linked backup stays outside retention. Deleting it returns `409 invalid_state`
while any setup links to it. Deleting the setup releases the link. Besides a rollback with
`world`, the world backup can be restored on its own through
`POST /instances/{id}/backups/{bid}/restore`.

### Send server commands

`GET /instances/{id}/capabilities` reports `command_channel`: `rcon` when the server has
the `Tristan-ValheimRcon` mod installed and enabled, otherwise `none`. `allowed_commands` lists
the commands the caller may send, and is empty for a caller with `commands.unrestricted`, which
may send any command. `commands.unrestricted` is held by administrators and cannot be granted.

`POST /instances/{id}/commands` with `{"command": "save"}` needs `commands.send` and a
running server, and returns `{"accepted": true, "output": "..."}` with the mod's reply.

| Error                   | Cause                                                               |
| ----------------------- | ------------------------------------------------------------------- |
| `409 unsupported`       | The server does not have the mod.                                   |
| `409 invalid_state`     | The server is not running.                                          |
| `422 validation_failed` | Empty, multi-line, over 1,024 bytes, or not allowed for the caller. |
| `429 rate_limited`      | Over 30 commands a minute per server, after a burst of five.        |
| `503 unavailable`       | The RCON connection failed.                                         |

### Run world tools

`GET /instances/{id}/capabilities` also reports `world_tools`:
`{"upgrade_world": true, "fresh_world": false}` says which of `JereKuusela-Upgrade_World`
and `sighsorry-FreshWorld` the server has installed and enabled. `valmin_eye` is the installed
version of `valminhq-ValminEye`, or `null` without it or while it is disabled. `packages`
names the mod behind each capability: `{"rcon": "Tristan-ValheimRcon", "upgrade_world":
"JereKuusela-Upgrade_World", "fresh_world": "sighsorry-FreshWorld"}`.

`POST /instances/{id}/world-tools` needs `world.tools`, which only administrators hold, and a stopped server with the
tool's mod and `Tristan-ValheimRcon` installed and enabled. It returns `202` with a `world_tool` job that
runs up to four steps in order: it backs up the world as a `pre_update` backup (left out when the
server has no world yet), starts the server, waits for the log line that says its world loaded (for
at most the ready timeout, three minutes by default, then goes on), and sends one command over RCON,
which it must deliver. The backup and the start are jobs of their own in the
job history, carrying the requester. The server keeps running afterwards. The job ends once the command
is sent, not when the mod finishes. A failed step ends the job with that step's reason, and the server
stays as the step left it. The job cannot be cancelled once it runs.

| `tool`          | `action`      | Extra field                     | Sends                              |
| --------------- | ------------- | ------------------------------- | ---------------------------------- |
| `upgrade_world` | `zones_reset` | `min_distance_m` (0 to 20000)   | `zones_reset start [min=<m>]`      |
| `upgrade_world` | `upgrade`     | `operation`, e.g. `tarpits`     | `upgrade <operation> start`        |
| `upgrade_world` | `world_clean` |                                 | `world_clean start`                |
| `fresh_world`   | `run`         |                                 | `freshworld`                       |

Operations ending in `_worldgen` are refused.

| Error                   | Cause                                                    |
| ----------------------- | -------------------------------------------------------- |
| `409 unsupported`       | The tool's mod or `Tristan-ValheimRcon` is not installed or is disabled. |
| `409 invalid_state`     | The server is not stopped.                               |
| `422 validation_failed` | An unknown tool or action, or a bad extra field.         |

### Edit settings and files

Omitted fields in a settings `PATCH` remain unchanged. Unknown JSON fields are
rejected. Some launch changes, a password reset and a setup restore set
`restart_required`; saving settings does not mean the running game has adopted them.
Config writes and mod installs, uninstalls and toggles set `pending_restart` instead,
which also reads `true` while a mod install is queued. A successful start clears both.
Only `restart_required` can raise an alert.

`game_branch` and `game_branch_password` choose the branch the next game update installs, as
in a create. They are applied together: a branch sent without a password clears the stored
one. Neither touches the running server nor sets `restart_required`. The password is never
returned.

`game_backups` is how many of its own rolling backups the game keeps, 1 to 50 (the game's
default is 4). It is a launch setting, so it sets `restart_required`.

`auto_stop_minutes` stops a running server once it has had no players for that many minutes.
It is `0` (off) or 5 to 1440, needs both `instance.settings` and `instance.stop`, and takes effect immediately without
setting `restart_required`. An unknown player count counts as occupied.

`watchdog` is `off` (the default), `alert` or `restart`: what Valmin does about a running server
whose log has shown no `Connections` status line for 15 minutes, or for 25 minutes after the
server started. `alert` sends one `instance_down` event per silence and leaves the server running.
`restart` also restarts it, at most once an hour per server (counted in memory), and only a server
that has logged a status line since it started; a server that has not is only reported. A restart
that has to wait, for the hour or for a job holding the server, is made when it can be, with a
second event. The restart job records `clean: false` when Docker had to kill the stopped server,
and the instance is started again unless Docker still shows the container running, in which case
it is held in the `error` state. Any other value returns `422`. It takes effect immediately
without setting `restart_required`.

`setup_before_update` is `full` (the default), `light` or `off`: what a mod install, removal or
update saves of the setup before it moves any file. `full` and `light` save an automatic setup,
full or light, linked to an update's `pre_update` backup; `off` saves none, and an update still
takes its backup. It needs `setups.manage`, takes effect immediately without setting
`restart_required`, and any other value returns `422`. A copy made to test an update gets `off`.

`setup_versions_kept` (1 to 100, default 10) is how many automatic setups the server keeps, and
`setup_worlds_kept` (0 to 100, default 3) how many of those, newest first, keep the world backup
they link. Lowering either removes the oldest automatic setups, or releases their backups to
normal retention, in the same request. Both need `setups.manage`, take effect without setting
`restart_required`, and a value out of range returns `422`.

`join_host` is the host name or IP address players connect to, without a port: an IP address,
or a DNS name of letters, digits and hyphens. Anything else, including a port, returns `422`.
An empty string clears it. It takes effect immediately without setting `restart_required`. A
server's JSON also carries two fields derived from it. `join_address` is the host with the game
port, such as `play.example.com:2456` or `[2001:db8::1]:2456`. `steam_join_url` is a
`steam://run/` link that starts the game and connects to `join_address`; it does nothing while the
game is already running. Both are `null` without
a join host, and `steam_join_url` is also `null` for an IPv6 host and while crossplay is on.

`backup_on_restart` makes a consistent backup after the stop of every restart, every auto-stop
and every stop a schedule fires, including the stop step of a schedule's chain. A stop a person
asks for, a planned power cut and the stop steps of an update test never back up. The backup is
skipped, and the job's log says so, when the stop did not confirm the world save or the data root
is below `data.free_space_floor_bytes`; the stop itself is never refused. It takes effect
immediately without setting `restart_required`.

The game password is never part of a server's JSON. Read it with
`GET /instances/{id}/password`, which returns `{"password": "..."}` and writes an
`instances.password.read` audit entry on each call. After a password change, a
running server keeps the previous password until it restarts.

`GET /instances/{id}/reachability` measures from the panel host whether players outside it can
find the server. It needs `instance.view` and answers `404` for a server the caller cannot see.
A call can make outbound requests, so a user is limited to five a minute (`429` with
`Retry-After` beyond that). A check that finds a problem still answers `200`, with the problem
in the body:

```json
{
  "status": "warn",
  "public_address": "203.0.113.7:2456",
  "detail": "The server did not answer at 203.0.113.7:2456 when asked from the panel host.",
  "remedy": "Forward UDP ports 2456-2457 on the router to this host ...",
  "steps": [
    {"name": "public_ip", "status": "ok", "detail": "The host's public address is 203.0.113.7."},
    {"name": "steam_listing", "status": "ok", "detail": "Steam lists this server at 203.0.113.7."},
    {"name": "server_query", "status": "warn", "detail": "No answer from 203.0.113.7:2457 (UDP) ..."}
  ],
  "checked_at": "2026-10-25T10:00:00Z"
}
```

`status` is `ok`, `warn` or `unknown`; `unknown` means the check could not measure, such as a
stopped server, a settings change waiting for a restart, or a public IP lookup that is turned off.
`public_address` is where players connect directly and is absent for a stopped server, for a
crossplay server, which joins by code, and when the host's address is unknown. `remedy` is absent
when `status` is `ok`. `steps` lists what was measured: `public_ip`, `steam_listing` and
`server_query`. The last two run only for a server with public listing on, because only then does
the server answer queries (and not with crossplay on, which the check does not test at all); for
any other server `server_query` is `unknown` and `steam_listing` is absent. When the public IP lookup fails or is turned off, `steps` holds only `public_ip`. The list
is empty when the server is stopped, uses crossplay or has a restart pending. The lookup address comes from
[`game.public_ip_url`](configuration.md#check-that-players-can-reach-a-server).

For raw configuration replacement, first fetch the file and retain its `ETag`.
Send that exact value as `If-Match` with the replacement text and
`Content-Type: text/plain`. A missing or stale
value returns `412 stale_write`; reload and reconcile the file before retrying.
A replacement that breaks a `.json` file's syntax (a leading UTF-8 byte-order mark is allowed)
or any document of a `.yaml`/`.yml` file returns `422 validation_failed` with the parser's
message on the field `content`. The check applies to every write, also when the file on disk
does not parse, so a file the mod reads leniently (JSON with comments) must be saved as strict
JSON. A `.cfg` replacement is held to BepInEx's line grammar (blank, `# comment`, `[Section]`,
`Key = value`) and a broken line returns the same `422` naming it, unless the file on disk
already broke that grammar. When the file on disk uses CRLF line endings, the replacement is
stored with CRLF too; if that takes it over 1 MiB it answers `413 payload_too_large`.

`{file}` is a path relative to `BepInEx/config`, with each `/` sent as `%2F`, for example
`/configs/expand_world%2Fexpand_biomes.yaml/raw`. It may sit up to 8 folders deep and must end
in `.cfg`, `.yaml`, `.yml`, `.json` or `.txt`; no folder may start with a dot. Any other name
returns `404`. Only `.cfg` files have settings: the typed read, `PATCH`, and the `/original`
and `/previous` settings routes return `404` for the others, which are read and written
through the raw routes. A raw write to a running server leaves a `.pending` copy only for a
`.cfg` file; any other file is replaced and stays as written. The audit entry of a write to a
file that is not `.cfg` lists no settings.

`GET /instances/{id}/configs` walks `BepInEx/config` and its subfolders. It skips hidden
folders, symlinked folders, symlinks that do not lead to a regular file inside the directory,
files with other extensions, files other than `.cfg` over 1 MiB and folders more than 8 deep,
and returns at most 5,000 files; when a limit left a file out, `note` says so. Each
item's `file` is its path, `dir` the directory it is relative to, and `portable` whether
template codes carry its settings once the panel changes them: only a `.cfg` directly in
`BepInEx/config`, other than the RCON plugin's. Server definitions carry the files the panel
changed, as described under Server definitions, and saved setups hold every listed file except
the RCON plugin's. `mod_replaced_edits` is
`{"mod", "version", "created_at"}` while the file's newest version is one a mod install,
update or downgrade wrote over bytes the mod had not shipped, and `null` otherwise.

`GET /instances/{id}/configs` lists each file's `installed_mods`: the installed mods it
belongs to, matched by name (a file in a subfolder also by that top folder, such as
`EpicLoot/`), or an empty list. `DELETE /instances/{id}/configs/{file}`
deletes the file with its `.pending` copy and any `.bak` or `.orig` copy an earlier release
kept, and answers `204`. The file's history, the deleted version included, is kept and is
served again once a file of that name exists; a file over 16 MiB is deleted without keeping it. It needs
`config.edit` for a `.cfg`, or `config.raw` for any other file, and a stopped or running
server. A file an installed mod uses is refused
with `409 mod_conflict` and `details.installed_mods`; send `allow_installed=true` to delete
it anyway. That mod's settings return to their defaults on the next start, so the server
is marked as pending a restart.

`PATCH /api/v1/instances/{id}` checks a permission per field:

| Fields                                                                                  | Permission                              |
| --------------------------------------------------------------------------------------- | --------------------------------------- |
| `name`, `server_name`, `password`, `public`, `crossplay`, `preset`, `modifiers`         | `instance.settings`                     |
| `game_backups`                                                                          | `instance.settings`                     |
| `status_published`, `status_notice`, `status_connect_info`                              | `instance.settings`                     |
| `status_show_join`, `status_show_modpack`                                               | `instance.settings`                     |
| `join_host`                                                                             | `instance.settings`                     |
| `backup_on_restart`                                                                     | `instance.settings`                     |
| `backup_keep_cold`, `backup_keep_hot`                                                   | `backups.restore`                       |
| `remote_backup_enabled`, `remote_keep_cold`, `remote_keep_hot`, `remote_keep_snapshots` | `backups.restore`                       |
| `auto_stop_minutes`                                                                     | `instance.settings` and `instance.stop` |
| `watchdog` set to `off` or `alert`                                                      | `instance.settings`                     |
| `watchdog` set to `restart`                                                             | `instance.settings` and `instance.restart` |
| `setup_before_update`, `setup_versions_kept`, `setup_worlds_kept`                       | `setups.manage`                         |
| `game_branch`, `game_branch_password`                                                   | `instance.update`                       |
| `mem_limit_mb`, `cpu_limit`                                                             | `instance.limits`                       |
| `extra_args`                                                                            | `instance.extra_args`                   |

The status page texts are plain text of at most 500 characters each. A retention count
of `0` keeps every archive of that type.

### Mod configuration files

`GET /api/v1/instances/{id}/configs/{file}` needs `config.read` and returns the file's
settings in file order:

```json
{
  "file": "com.example.plugin.cfg",
  "plugin": "Example Plugin",
  "sections": [
    {
      "name": "General",
      "settings": [
        {
          "key": "Enabled",
          "type": "Boolean",
          "description": "Turn the plugin on or off.",
          "default": true,
          "current": true,
          "range": null,
          "options": null,
          "widget": "toggle",
          "step": 0
        }
      ]
    }
  ]
}
```

Values are JSON booleans and numbers where the setting's declared type says so, and
strings otherwise. `range` holds `min` and `max` for a bounded number, and `options` the
choices of a fixed set. A type the panel does not recognise comes through as a string.

`PATCH /api/v1/instances/{id}/configs/{file}` needs `config.edit` and takes
`{"Section.Key": value}` pairs. Every value is validated before any is written, so one
bad value leaves the file untouched and answers `422` with the field named. Only the
changed values are rewritten; comments and layout stay. `If-Match` with the file's `ETag`
is optional and, when sent, refuses a stale write with `412 stale_write`. The response
is the updated settings with a new `ETag`.

The panel keeps a history of each file. Before a write it records the bytes on disk, labelled
`outside` when they differ from the newest kept version, and after it the bytes it wrote. A
write whose bytes equal the file's writes nothing, and a write to a file over 16 MiB, too large
to keep, is refused with `413 payload_too_large`. The RCON plugin's file has no history. `GET .../history` lists the kept versions,
newest first, in `items` with a `next_cursor` that is always `null`: the newest 20 plus the
baseline. Each has `id`, `created_at`, `source` (`found`, `outside`, `edit`, `setup` or
`mod`), `user` for an edit, `mod`, `from_version` and `to_version` for a mod install, update
or downgrade (`from_version` is `null` for a first install), `replaced_edits` for a mod
version that replaced bytes the mod had not shipped, `baseline` and `size_bytes`.
`GET .../history/{rev}` returns one `.cfg` version's settings plus `captured_at`, and
`.../history/{rev}/raw` its bytes, which needs `config.raw`. An id the file does not have is
`404`.

`/original` serves the baseline: the first version recorded, or the one a mod install last
wrote. `/previous` serves the newest kept version that differs from the file. Both return the
settings plus `captured_at`, or `404` when there is none. A file with no history yet serves the
`.orig` and `.bak` copies earlier releases kept beside it. `/original/raw` and
`/previous/raw` return their bytes and need `config.raw`, like the live `/raw`.

Writes are allowed on a stopped or running server. A write to a running server also
keeps a pending copy, whose values the panel puts back after the server stops, because
some plugins save their loaded settings over the file at shutdown.

### Worlds and player lists

`POST /api/v1/instances/{id}/worlds/import` needs `world.import` and a stopped server. Send
the world as `multipart/form-data`, one `file` part per file, with each part's filename
set to its path inside the world folder. A ZIP of the folder, a `.tar.gz` or `.tgz` such
as a world backup the panel wrote, or the `.db` and `.fwl` pair of an older save, works
too. A `.tar.gz` that is truncated or corrupt answers `400 invalid_parameter` rather than
importing part of its world, as does one that expands to more than the 4 GiB upload limit.
The game's rolling saves inside it are ignored, and an archive holding more than one world
is refused like any other upload of several. The files are renamed to the server's world name, and
the current world is archived first. The game's own older saves, such as `.old` files,
are refused unless the request adds `allow_backup_variant=true`. A server takes one upload at
a time: a second upload while one is still being received answers `409 job_in_progress`. An
upload whose size would leave the data directory below its free-space floor answers
`409 insufficient_disk` before anything is written.

`GET /api/v1/instances/{id}/worlds` marks each of the game's rolling saves with
`"game_backup": true`. `POST /api/v1/instances/{id}/worlds/{name}/restore` loads one of them,
or any other complete world in the list, in place of the current world, which is archived
first. It needs `world.import` and a stopped server.

`POST /api/v1/instances/{id}/worlds/import-backup` takes a world out of a backup archive
that is already on the host and installs it as this server's world, as an upload would. It
needs `world.import` on the target and a stopped server, and the body is JSON:
`source_instance_id` and exactly one of `backup_id` or `filename`, plus an optional `world`.

- With `backup_id`, the archive is that backup of `source_instance_id`. The caller needs
  `backups.download` on the source, because the world leaves it; a source the caller
  cannot see answers `404`.
- With `filename`, the archive is a leftover of a server that no longer exists, named as
  `GET /api/v1/backups/orphaned` lists it. Only administrators may use it; anyone else, and
  any name the listing does not hold, gets `404`. The name is matched against the listing
  and never used as a path.

A backup holds every world its server kept, and one is imported. `world` names it. Without
it the world the backup's own row names is taken if the archive holds it, and otherwise the
only world there is. An archive with several worlds and no way to choose answers
`422 validation_failed` with the candidates in `details.worlds`; send the request again with
`world`. The current world is archived first, the source archive is left as it was, and the
rest of the response is the same as for an upload. The archive is expanded inside the
request, which is why a large one takes a while to answer `202`. A source archive that is
missing answers `404`, and one that cannot be read `422`.

`GET /api/v1/backups/orphaned` needs the administrator's adopt action (`403` otherwise) and
lists the finished `.tar.gz` archives under `backups/<id>/` for which no server `<id>`
exists, newest first, as `instance_id`, `filename`, `size_bytes` and `modified_at`. An
archive still being written is not listed.

The three player lists are `admins`, `bans` and `permitted` under
`/api/v1/instances/{id}/`. Each needs `players.manage`. `GET` returns `{"ids": [...]}`
with an `ETag`. `PUT` takes the same body and replaces the whole list; it requires
`If-Match`, answers `412 stale_write` when someone else changed the list, and returns the
saved list with a new `ETag`. Comments already in a file are kept. The game reads the
files while it runs; nothing restarts.

`GET /api/v1/instances/{id}/players/seen` also needs `players.manage` and lists the
accounts the server named in its log, most recent first, each with `platform_id`, `name`,
`first_seen_at`, `last_seen_at` and `wrong_password_at`. The ID is printed as the server
printed it, which is the form the player lists take. `wrong_password_at` is when the
server refused the account's latest attempt for a wrong password, and `null` otherwise.
`names` lists every name the account was seen with, the ones to believe first, each with
`name`, `exact` (read beside the ID), `hits` (spawns matched by timing) and `likely`.

`GET /api/v1/players` lists every account any server has seen, most recent first, each
with `platform_id`, `names` (as above, with hits summed over the servers), `last_seen_at`
and `servers`: `instance_id`, `instance_name`, `name`, `first_seen_at` and `last_seen_at`
per server. It needs `players.manage` held globally, which only administrators have;
everyone else gets `404`.

### Notifications and alert rules

Destinations and rules are served in two scopes, with the same routes under two prefixes.
`/admin` needs `panel.settings`, and everyone else gets `404`. `/instances/{id}` needs
`notifications.manage` on that server: `404` for a server the caller cannot see, `403` for
one they can see without it. Paths are relative to `/api/v1`, and `{scope}` is `/admin` or
`/instances/{id}`.

| Method   | Path                               | Purpose                                                    |
| -------- | ---------------------------------- | ---------------------------------------------------------- |
| `GET`    | `{scope}/webhooks`                 | Destinations.                                              |
| `POST`   | `{scope}/webhooks`                 | Add a destination; returns `201`.                          |
| `PATCH`  | `{scope}/webhooks/{wid}`           | Change `name`, `url` or `enabled`.                         |
| `DELETE` | `{scope}/webhooks/{wid}`           | Delete a destination and remove it from every rule; `204`. |
| `POST`   | `{scope}/webhooks/{wid}/test`      | Send a test notification; returns a job. Rate-limited.     |
| `GET`    | `{scope}/webhooks/deliveries`      | Delivery records, newest first.                            |
| `GET`    | `{scope}/alert-rules`              | Alert rules.                                               |
| `POST`   | `{scope}/alert-rules`              | Add a rule; returns `201`.                                 |
| `PATCH`  | `{scope}/alert-rules/{rid}`        | Change a rule.                                             |
| `DELETE` | `{scope}/alert-rules/{rid}`        | Delete a rule; returns `204`.                              |
| `POST`   | `{scope}/alert-rules/test`         | Send the message a rule would; returns a job. Rate-limited. |

A destination is shared or belongs to one server. `/admin` lists every destination and adds
shared ones. A server's prefix lists the shared destinations and the server's own, adds
destinations owned by the server, and changes, tests or deletes only those: a shared or
another server's destination there is `404`. Likewise `/admin` lists and changes every rule,
and a server's prefix lists the server's rules and the rules for every server but changes
only the server's own. A rule created there is about the server; naming another
`instance_id` is `422`. Deliveries under a server's prefix are only those about that server.

A destination has `name`, `kind` (`discord` or `generic`), `url`, `enabled` and `instance_id`
(the server it belongs to, `null` when shared), and receives only what the rules naming it
send. A `catch_all` field in a request is accepted and ignored. `kind` and `instance_id` never
change. A name is
unique among the shared destinations and among each server's own; a taken one is `409
name_taken`. The URL is write-only: no response contains it, because it is the whole credential for posting to
that channel. A URL the panel refuses, such as a private address, is `422` on `url`.
Deliveries can be narrowed with `webhook_id`, `rule_id` and `status` (`pending`,
`delivered` or `failed`). Each record has the event kind, the server, the number of
attempts and the last error, which never contains the URL. A delivered or failed record is
kept for 30 days; the records of an alert are kept until 30 days after it resolves.

A rule has `condition_kind`, `instance_id` (`null` for every server), `webhook_ids`,
`enabled`, `params` and the quiet hours fields described below. Every webhook a rule names
must be shared or belong to the rule's server, so a rule for every server names only shared
ones; any other is `422` on `webhook_ids`, worded as if it did not exist. The condition kinds are
`job_failed`, `job_stuck`, `crash_loop`, `unclean_stop`, `instance_error`,
`restart_required`, `update_available`, `stale_backup`, `backup_attention`, `mods_failed`,
`mod_updates` and `low_disk`, plus the one-off events `server_started`, `server_stopped`,
`instance_down`, `backup_failed`, `game_updated`, `new_game_build`, `update_test_finished`,
`power_cut` and `sign_ins_blocked`, and the game events below.

`sign_ins_blocked` is sent when an account's failed sign-ins from any address reach the
limit and the panel starts refusing it. It is sent at most once an hour for each username, and
only for a username an account has, with the `username` and the `address` of the first refused
attempt. It is host-wide, like `low_disk`, `power_cut` and `new_game_build`.

Alert rules (`/api/v1/admin/alert-rules`) route a condition kind to chosen destinations and
send `alert_opened` and `alert_resolved`.

The one-off `condition_kind` values are events rather than conditions, sent only to the
destinations of an enabled rule for them. `server_started` sends `server_started` when a
start or restart leaves a server running, once its log says the world loaded and, on
crossplay, once it has a join code; it is sent without them after 15 minutes.
`server_stopped` sends `server_stopped` when the panel stops a server, by hand, by auto-stop or for a power cut, with a `Reason` detail; a
server that stops on its own is `instance_down`. Rules of the old `auto_stopped` kind became
`server_stopped` rules. `instance_down` sends `instance_down` when a server stops or restarts
on its own; after a restart outside the panel the `Status` detail reads `Running again`.
`backup_failed` sends `backup_failed` for every backup that fails, recorded with the job's
outcome, with the reason and the job id; `job_failed` also covers backups but announces a
failure once, until a later run of the job succeeds. `new_game_build` (host-wide) sends
`update_available` once for each public build the panel observes for the first time, with
`build`; the `update_available` condition is per server instead. `game_updated` sends `game_updated` when a game update
installs a build other than the one the server ran, with `build`, never for a copy made to test
updates, and when applying tested updates updates mods, with
`mods`; an apply that also installs a build sends the build in a message of its own.
`update_test_finished` sends `update_test_finished` when an update test ends, by hand or on a
schedule, unless it was cancelled: `status` says whether it failed, whether every mod loaded,
and whether the updates are being applied or why an apply the schedule asked for is not;
`build` is the game build the copy moved to, `mods` the mod updates on it and `failed_mods`
the packages that failed to load. `power_cut` (host-wide, so its rule names no server) sends `power_cut_soon` once per planned power cut, when players are warned (`warn_at`), with
the cut time, the stop time and the running servers. Inside a rule's quiet hours they are
not sent at all. Alerts held during a
rule's quiet hours are sent when the window ends. A rule sends `alert_resolved` only for an
alert it opened. To a Discord destination, `alert_resolved` edits the message of the
`alert_opened` it resolves, through the webhook's message edit endpoint, when that message
was delivered and Discord still has it; otherwise it is posted as a new message. Discord
messages are posted with `wait=true`, so the panel learns each message's id.

Seven `condition_kind` values are game events that the
[ValminEye](https://github.com/valminhq/valmin-eye) plugin writes to the server's log:
`player_joined`, `player_left`, `boss_defeated` (a boss defeated for the first time in the
world), `boss_fight_started`, `boss_fight_ended`, `raid_started` and `raid_ended`. Each is
sent under the same kind name, only to the destinations of an enabled rule for it, and not
during its quiet hours. `{players}` is the player list as `Alex (2 deaths), Bo`. A player's `player_joined`
and `player_left` on one server are sent at most once a minute. The event's fields are also
in `detail`, under their own names.

Every rule shapes its message with these `params`:

| Field       | Meaning                                                                                                                       |
| ----------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `template`  | The message, at most 1000 characters (`422` with `out_of_range`). Absent sends the kind's default.                             |
| `layout`    | Absent: a Discord card titled `Server · Headline`. `message`: a card with the message only. `plain`: the message as plain text. Anything else is `422` with `not_an_option`. |
| `hide_link` | `true` leaves the link off the card's title.                                                                                  |
| `hide_time` | `true` leaves the time off the card.                                                                                          |
| `color`     | The card's colour as `#RRGGBB` (`422` with `invalid` otherwise). Absent is the kind's own.                                     |

A `{name}` placeholder in a template is replaced by the event's value of that name, `{server}`
by the server's name and `{event}` by the headline. An unknown name becomes empty, and a line
whose every placeholder came out empty is left out. Values are inserted with Discord markdown
escaped, so a player name cannot add formatting or a link; the template's own markdown is
kept. The names each kind fills:

| Kind               | Names                                                         |
| ------------------ | ------------------------------------------------------------- |
| `job_failed`       | `reason`, `what_to_do`                                        |
| `low_disk`         | `free`, `alarm`, `what_to_do`                                 |
| `stale_backup`     | `schedule`, `last_backup`, `what_to_do`                       |
| `unclean_stop`     | `job`, `what_to_do`                                           |
| `restart_required` | `what_to_do`                                                  |
| `update_available` | `installed`, `available`, `what_to_do`                        |
| `instance_error`   | `reason`, `what_to_do`                                        |
| `crash_loop`       | `stops`, `what_to_do`                                         |
| `job_stuck`        | `running`, `what_to_do`                                       |
| `backup_attention` | `backup`, `findings`, `what_to_do`                            |
| `mods_failed`      | `mods`, `reason`, `what_to_do`                                |
| `mod_updates`      | `count`, `mods`, `what_to_do`                                 |
| `server_started`   | `server_name`, `world`, `join_code`, `port`, `time`           |
| `server_stopped`   | `reason`, `server_name`, `world`, `time`                      |
| `instance_down`    | `status`, `reason`, `what_to_do`, `time`                      |
| `backup_failed`    | `reason`, `what_to_do`, `job_id`                              |
| `game_updated`     | `build`, `mods`, `time`                                       |
| `new_game_build`   | `build`, `what_to_do`                                         |
| `update_test_finished` | `status`, `build`, `mods`, `failed_mods`, `time`          |
| `power_cut`        | `power_off`, `stop_at`, `servers`                             |
| `sign_ins_blocked` | `username`, `address`                                         |
| Game events        | The event's own fields, as listed for ValminEye above         |

`what_to_do` is empty on `alert_resolved`. A field named in `hidden_fields` is empty too.

A generic payload carries `version`, `id`, `kind`, `occurred_at`, `headline`, `instance`
and `detail`, plus `text` (the message of an event a rule sent, as plain text without
Discord markdown or escapes), `resolves` (on
`alert_resolved`, the `id` of the `alert_opened` it resolves, when known) and `url` (a link to the server's page, or to the panel for a host-wide event)
when `server.external_url` is set. `detail` maps readable labels to readable values, such as
`"Free space": "4.5 GB"` and `"What to do": "…"`. It is written for people, so a receiver
should display it rather than parse it. A Discord embed's title is the server name and the
headline, linking to the same page. A rule's message is the embed's description, and the
embed then has no fields.

`stale_backup` opens when a backup schedule has fired `stale_factor` times (2 by default),
counted on its own clock and days, since the last consistent archive; a hot copy does not count. On a server with
`backup_on_restart`, its restart schedule counts too, while its latest restart succeeded. A failed
restart raises `job_failed` instead. A schedule that only stops the server does not count.

`instance_error` carries the observer's `reason` when the server was parked for something it
saw, such as `container was killed by the out-of-memory killer; the world may be damaged`, and
no job finished since; an error a job left behind is that job's `job_failed`. The detail also
holds `MemLimitMB`, the server's memory limit, 0 for none.

`mods_failed` opens when the BepInEx log of a modded server's latest boot names mods it could
not load, with `Mods` (a dependency skipped because another mod failed reads `(skipped)`) and
the loader's `Reason` for the first one; it resolves on a boot that loads them all. A successful
start or restart runs a scan at once, unless one is already running; the five-minute scan
follows either way. It is
`critical` in the inbox.

`mod_updates` opens when the mod catalogue has newer versions of a server's mods, with
`Count` and `Mods` (`Name 1.2.0 → 1.3.0`), and resolves once none are pending. It is evaluated
only for servers an enabled `mod_updates` rule covers, so without a rule it never appears.

`backup_attention` opens when a server's newest archive other than a snapshot was flagged by
the [world check](operations.md#world-check), and resolves when a newer archive passes or the
flagged one is acknowledged. In the backup catalog, `world_bytes` is the size of the world
inside the archive (`null` when never measured) and `world_attention` is a list of
`{code, message}` findings, empty when there are none. The codes are `world_shrunk`,
`db_shorter_than_old`, `save_incomplete`, `version_mismatch`, `chunk_missing` and
`db2_damaged`; `message` is written for people. A finding never refuses an archive.
`kept_for_setup` is `true` while a saved setup links the archive, which keeps it out of
retention and makes deleting it `409 invalid_state`; `linked_setups` lists those setups as
`{id, name}` for a caller with `setups.manage` and is absent otherwise.

Each rule sends its own message: a destination named by rules of different kinds, such as
`job_failed` and `backup_failed`, gets one from each. A destination named by several rules of
one one-off kind gets that event once, rendered by a rule about the server when one names it,
otherwise by a rule for every server.

`{scope}/alert-rules/test` takes `condition_kind`, `params`, `webhook_id` and `fields`, the
sample values to fill the template with (at most 16, each a lowercase name and a value of at
most 200 characters). It sends the message a rule with those `params` would, as a `test`
event, to one destination the scope's rules may use, and stores nothing but the delivery. It
returns `202` with the job. A destination the scope's rules cannot use is `422` on
`webhook_id`.

Both test endpoints share one limit of five sends a minute per user; past it they answer
`429 rate_limited` with `Retry-After`.

Thresholds live in `params`: `crash_count`, `crash_window_seconds` and
`stuck_after_seconds` must be 0 or more, and `stale_factor` must be 0 or above 1. Zero or an
omitted field means the default; a value outside these ranges, or a duration too long to hold,
returns `422` with `out_of_range` on `params.<field>`. `params.hidden_fields` lists message
fields a `server_started` rule (`server_name`, `world`, `join_code`, `port`, `time`) or a
`server_stopped` rule (`reason`, `server_name`, `world`, `time`) leaves out, and
`params.timezone` is the IANA zone the rule's messages show times in (UTC when absent); either
one invalid returns `422` with `not_an_option`. Sent `params` replace the stored ones.
Quiet hours (`quiet_start_minutes`, `quiet_end_minutes`, minutes from midnight, and
`quiet_timezone`) are sent together; an empty `quiet_timezone` clears them. Alerts still open
when quiet hours end are sent then; one that opens and clears inside the window is not sent.
On a `PATCH` under `/admin`, an empty `instance_id` clears the rule's server and a `null` one
is ignored. Creating, changing and deleting a rule writes an audit log entry with the
operation `alert_rule_create`, `alert_rule_update` or `alert_rule_delete`, and a rule test
`alert_rule_test`. Under `/admin` its action is `panel.settings`; under a server's prefix it
is `notifications.manage`, with the server. Destination changes are audited the same way.
Deleting a rule that does not exist returns `404`. A `low_disk`, `power_cut` or `sign_ins_blocked` rule is
host-wide: naming an `instance_id` on one, or creating one under a server's prefix, returns
`422`.

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

### Users and invites

Account management needs `users.manage`, and invites `invites.manage`; both are
administrator-only, and everyone else gets `404`. Paths are relative to `/api/v1`.

| Method   | Path                         | Purpose                                                  |
| -------- | ---------------------------- | -------------------------------------------------------- |
| `GET`    | `/users`                     | Every account.                                           |
| `POST`   | `/users`                     | Create an account; returns `201`.                        |
| `PATCH`  | `/users/{id}`                | Change `role` or `disabled`.                             |
| `DELETE` | `/users/{id}`                | Delete an account; returns `204`.                        |
| `POST`   | `/users/{id}/password/reset` | Set a generated password and return it once.             |
| `GET`    | `/invites`                   | Every invite, with its state.                            |
| `POST`   | `/invites`                   | Create an invite; returns `201` with its token and link. |
| `DELETE` | `/invites/{id}`              | Revoke an invite; returns `204`.                         |
| `POST`   | `/invites/{token}/redeem`    | Create an account from an invite. Needs no session.      |

An account has `id`, `username`, `role` (`admin` or `member`), `disabled`, `owner`,
`created_at`, `last_login_at` and `password_change_required`. `POST /users` takes `username`, `password` (at least
eight characters) and `role`; a taken username is `409 name_taken`. The owner, the
account created at first-run setup, cannot be demoted, disabled or deleted, and only the
owner can reset its password: those requests answer `403`. Usernames are case-sensitive. A password reset returns `{"password": "..."}` and ends every
session of the account. Disabling an account or changing its role also ends its sessions.

`POST /invites` takes `instance_id`, `grant_role` (`viewer` or `operator`) and
`grant_perms`, the extra capabilities. Omit all three for a member with no server yet.
Capabilities only administrators can hold are refused with `422`. The response has
`token`, `url` and `expires_at`, and the token is never shown again. Invites expire after
seven days by default (`auth.invite_ttl`).

Redeeming takes `{"username": "...", "password": "..."}`, creates a member with the
invite's grant, signs it in, and returns the account with the session cookies. An
expired, revoked, used or unknown token answers `410 invite_invalid`, whichever it is.
The route is rate limited per client address.

### Audit log

Only administrators can read the audit log; everyone else gets `404`. Entries are kept
permanently and record the actor's and server's names as they were when the entry was
written, so renaming or deleting either does not change history.

| Method | Path             | Purpose                                                  |
| ------ | ---------------- | -------------------------------------------------------- |
| `GET`  | `/audit`         | Entries, newest first, with the usual cursor pagination. |
| `GET`  | `/audit/filters` | The actions, actors and servers that appear in the log.  |
| `GET`  | `/audit/export`  | Every entry matching the filters, as `audit-log.csv`.    |

If a later page of the export cannot be read, the connection is cut, so a short download
fails instead of looking complete.

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
`instances.delete`, `instances.create`, `instances.game.update`,
`instances.update_test.start`, `instances.update_test.apply`, `instances.backups.create`,
`instances.backups.restore`, `instances.backups.delete`, `instances.setups.save`,
`instances.setups.restore`, `instances.setups.delete`, `instances.worlds.import`,
`instances.worlds.restore`, `instances.worlds.delete`, `instances.mods.install`,
`instances.mods.update`, `instances.mods.uninstall`, `instances.mods.enable`,
`instances.mods.disable`, `instances.mods.lock`, `instances.mods.unlock`,
`instances.settings.update`, `instances.configs.write`, `instances.configs.delete`,
`instances.commands.send`, `instances.world_tools.run`,
`schedules.create`, `schedules.update`, `schedules.delete`, `schedules.run` and `jobs.cancel`.
Runs the schedule clock starts and jobs the panel resumes after a restart are not entered,
because nobody requested them.

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
for display. Include `request_id` when investigating the matching daemon log. A request body
with a field the endpoint does not accept answers `400 malformed_json` with the field in
`details.field`, as does a refusal that concerns one field, such as `409 name_taken`.

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

| Topic                   | Payload                                                 |
| ----------------------- | ------------------------------------------------------- |
| `instance.{id}.console` | Game log lines.                                         |
| `instance.{id}.stats`   | Resource samples.                                       |
| `instance.{id}.state`   | Server state changes, `maintenance` and `mods` signals. |
| `job.{id}`              | Job progress and status.                                |

The server checks permission per topic and replies with `subscribed` or `error`.

A `state` message is also sent, with the state unchanged, when a job on the server starts or
finishes and when its setup operation is abandoned. Read `GET /instances/{id}/jobs` and
`GET /instances/{id}/operation` again on each one.

`{"type":"mods","instance":"ID"}` means the mod install queue submitted an install, or one
finished with nothing queued after it. Read `GET /instances/{id}/mods` and
`GET /instances/{id}/mods/queue` again.
To stop listening, send `{"type":"unsubscribe","topics":["job.JOB_ID"]}`.

After subscribing to a job, fetch its HTTP resource so a job that already finished
is not missed. Fetch current state again after reconnecting. Console replay is
bounded, and console/stat streams can lose samples under load, marked by a `gap`
message. State and job messages are never dropped: a client that falls too far behind
on them is disconnected with code `1011`, and should reconnect and fetch again. Do not
treat the socket as a durable event log.

The socket closes with code `4401` when its session ends: sign-out, an account change,
or the idle or absolute expiry. Socket traffic does not count as activity, so a tab that
only watches a console is signed out at the idle timeout. Sign in again and reconnect.

A session holds at most 8 sockets. The ninth closes that session's oldest with code `1008`,
which also ends a connection for a policy violation, such as a bad origin or CSRF token. Do
not reconnect after `1008`. An account holds at most 32 sockets across all its sessions,
and past that its oldest closes the same way.

### Stats samples

`GET /instances/{id}/stats` and the `instance.{id}.stats` topic carry the same sample. While
the server runs, a sample is taken every two seconds while anyone is subscribed to the topic
and every 30 seconds otherwise, so the HTTP read can be up to 30 seconds old:

| Field | Meaning |
| --- | --- |
| `ts` | When the sample was taken. |
| `cpu_pct` | CPU use since the previous sample, 100 per core; null on the first sample. |
| `mem_bytes`, `mem_limit`, `mem_pct` | Memory in use, the container's limit, and their ratio; `mem_pct` is null without a limit. |
| `rx_bytes_per_second`, `tx_bytes_per_second` | Network traffic received and sent since the previous sample, over every interface; null on the first sample and after a restart resets the counters. |
| `players` | Current player count, or null when the panel cannot tell. |

The HTTP read adds `available`, false with every other field null while nothing is
sampled; the socket message adds `type` `stats` and `instance`.

## Health and public status

These routes are outside `/api/v1` and do not require a session:

| Method and path           | Response                                                                                                      |
| ------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `GET /healthz`            | HTTP 200 with `ok` when the daemon can respond.                                                               |
| `GET /readyz`             | JSON with `ready` and `components`; HTTP 503 if draining, SQLite is unavailable, or Docker cannot be reached. |
| `GET /public/status/{id}` | Server status only when its public status page is enabled.                                                    |

`GET /public/status/{id}` returns `name`, `online`, `players` and `observed_at`, plus `notice`,
`connect_info` and `activity` when set. With `status_show_join` on it adds `join_address` and
`steam_join_url`, or `join_code` for a crossplay server once the server has logged one. With
`status_show_modpack` on it adds `modpack_code` and `modpack_registry` while the stored profile
code matches the server's mods. Absent fields are omitted. The route never uploads a code and
never returns the password.

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
treat it as a missing answer rather than a pass. A check can carry `docs`, a link to the
guide that explains its remedy. A failing check can also carry
`diagnostic`, the verbatim output of whatever was probed. That field is present in the API
response and absent from the support bundle, which omits filesystem paths and replaces
server names, custom image references and a custom Thunderstore URL. Each live probe
gets five seconds, so an engine or registry that never answers fails its own check and
the report still arrives. A stored fact that cannot be read leaves its check failing or
`unknown`; the report itself fails only when the server list cannot be read. A check
recorded by the deep checks has `source` `job`, and one recorded at startup `startup`.

Server entries include nullable `exit_code`, `oom_killed`, `restart_count`, and
`finished_at` values, plus `server_free_bytes` from the last game telemetry sample.
`image` is the image the server's container runs, empty when it was not inspected.
`image_issue` says when that differs from `game.image`, and `runtime_issue` when the container
runs under another OCI runtime than `game.runtime`.
`mods_error` and `inspection_error` explain failed reads; clients must not interpret
the associated zero or empty values as successful measurements. The bundle replaces
these errors with generic messages.

`failed_jobs` lists the latest failed terminal job for each server and operation type,
including global jobs. Each item contains `id`, `kind`, and an optional `instance_id`.
Fetch `/jobs/{id}` for details.

Registry checks include reachability and a separate `.sync` check for each enabled
registry. Disabled registries report their configuration without making a request.

These three routes answer 403 to an account without the permission, not 404.

## Encryption keys

`POST /api/v1/admin/keys/rotate` needs `panel.settings` and returns a job that
re-encrypts every stored secret under a new key generation derived from the same master
key. No server stops and nothing has to be entered again. An interrupted rotation
finishes what is left when run again. It does not replace `secret.key`. An account
without the permission gets `404`.

## Installation export

Every route needs `panel.settings`; an account without it gets `404`.

- `POST /api/v1/admin/export` takes `{"mode": "light"}` or `{"mode": "full"}` and returns a
  job that writes an [installation export](operations.md#move-to-a-new-host). The job holds
  the lock of every server that is not running, so it answers `409` while a job runs on one,
  and jobs submitted for one while it runs get `409` too. The job can be cancelled at any
  point, and fails when the data root falls below its free-space floor.
- `GET /api/v1/admin/export` returns `{"latest": ...}`, the latest finished export with
  `filename`, `mode`, `size_bytes` and `created_at`, or `null` when there is none.
- `GET /api/v1/admin/export/download` returns that archive as `application/gzip`, with
  range requests supported, and `404` when there is none. Each download is written to the audit log.

## Cache

Both routes need `panel.settings`; an account without it gets `404`. What counts as in use
is described in [Reclaim cache space](operations.md#reclaim-cache-space).

- `GET /api/v1/admin/cache` lists the cached mod archives, game builds, saved setup files
  and partial downloads in one page (`next_cursor` is always `null`), with a `summary` per
  kind and `grace_seconds`, the time an unused entry is kept after it was last written or
  used. Each item has `kind` (`mod_archive`, `game_build`, `setup_blob` or
  `partial_download`), `name`, `registry` (set for mod archives and their partial
  downloads), `size_bytes`, `reclaimable_bytes`, `in_use`, `prunable` (a prune now
  removes it), `servers` (`id` and `name` of the servers using it), `setups` (the saved
  setups holding it) and `prunable_at` (`null` while in use). `reclaimable_bytes` is what
  a prune now would free: `0` for an entry in use or inside its grace period, and for a
  file whose bytes another name still links, so a mod archive and the saved setup file
  sharing its bytes count once.
- `POST /api/v1/admin/cache/prune` returns a job that removes every unused entry past its
  grace period. The job holds every server's instance and sequence locks, the saved-setup
  lock and the export lock, so it answers `409` while any of those jobs runs, and they get
  `409` while it runs. It is written to the audit log and cannot be cancelled; it takes
  seconds. While a game build download is in progress it leaves the game builds for a
  later prune and says so in its log.

```json
{
  "items": [
    {
      "kind": "mod_archive",
      "name": "denikson-BepInExPack_Valheim-5.4.2202",
      "registry": "thunderstore",
      "size_bytes": 798720,
      "reclaimable_bytes": 798720,
      "in_use": false,
      "prunable": true,
      "servers": [],
      "setups": 0,
      "prunable_at": "2026-11-24T10:00:00Z"
    }
  ],
  "next_cursor": null,
  "total": null,
  "summary": [
    { "kind": "mod_archive", "count": 1, "size_bytes": 798720, "reclaimable_bytes": 798720 },
    { "kind": "game_build", "count": 0, "size_bytes": 0, "reclaimable_bytes": 0 },
    { "kind": "setup_blob", "count": 0, "size_bytes": 0, "reclaimable_bytes": 0 },
    { "kind": "partial_download", "count": 0, "size_bytes": 0, "reclaimable_bytes": 0 }
  ],
  "grace_seconds": 3600
}
```

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

## See and end your sessions

`GET /api/v1/me/sessions` lists the signed-in account's own sessions that have not expired,
most recently used first. Each item has `id`, `created_at`, `last_seen_at`, `ip`,
`user_agent` and `current`, which is `true` for the session making the request. The
list has no further pages, so `next_cursor` is `null`.

`DELETE /api/v1/me/sessions/{id}` ends one of them: the session's cookie stops working, its
WebSockets close, and the call returns `204`. A session that does not exist, or that belongs to
another account, answers `404 not_found`. Ending the current session signs the caller out
without clearing their cookies; `POST /auth/logout` is the way to sign out here. Both calls
need the session cookie, and the `DELETE` needs the CSRF header. A revoke writes an audit
entry with action `users.session.revoke` and the caller's id as `target_user_id`.

## Set your time zone and time format

`PATCH /api/v1/me` changes the signed-in account's own preferences and returns the account,
as `GET /auth/me` does. It needs the session cookie and the CSRF header, takes no user id,
and needs no permission. A field left out is unchanged.

```json
{
  "timezone": "Europe/Berlin",
  "hour_cycle": "h23",
  "date_order": "dmy"
}
```

`timezone` is an IANA zone name. The SPA shows times in it and creates new schedules in it.
An empty string clears it, and the SPA then follows the zone the browser reports.
`hour_cycle` is `h23` for a 24-hour clock or `h12` for a 12-hour one. `date_order` is `dmy`,
`mdy` or `ymd`, the order the SPA writes a date's day, month and year in. An empty string in
either follows the browser's language settings.

`GET /auth/me` returns all three fields on every account, `""` when one is not set.

| Status | Code                | Cause                                                                     |
| ------ | ------------------- | ------------------------------------------------------------------------- |
| `401`  | `unauthenticated`   | There is no valid session.                                                |
| `422`  | `validation_failed` | `timezone` is `invalid`: not a zone the panel knows, or `Local`.          |
| `422`  | `validation_failed` | `hour_cycle` or `date_order` is `not_an_option`: not one of those values. |

## Passwords an administrator issued

Every account object, from `GET /auth/me`, the login response, `GET /users`, `POST /users`,
`PATCH /users/{id}` and `PATCH /me`, carries `password_change_required`. It is `true` for an
account an administrator created, and after an administrator resets its password, and `false`
otherwise: for the first administrator, for an account created from an invite, after the
account's own `POST /me/password`, after `valmind admin reset`, and for a member signed in by a
trusted proxy. The flag is advisory. The panel's own pages send a signed-in account that has it
to the change password page, but the API does not refuse any other request because of it.

## End the session

```sh
curl --fail-with-body --silent --show-error \
	--cookie cookies.txt --cookie-jar cookies.txt \
	--header "X-CSRF-Token: $VALMIN_CSRF" \
	--request POST \
	"$VALMIN_URL/api/v1/auth/logout"
rm login.json cookies.txt
```

Logout invalidates the session and returns 204, or 200 with `redirect_url` when a proxy
logout URL is configured. Remove the example's local files
when finished; they contain credentials.

## Remote backups

Destination administration requires `panel.settings`:

- `GET /api/v1/admin/remote-backup-destination`: active configuration or null.
- `PUT /api/v1/admin/remote-backup-destination`: kind (`webdav` or `rclone`),
  enabled, endpoint, username, remote_name, folder, and optional write-only password.
  An omitted password preserves the existing credential for the same destination.
- `POST /api/v1/admin/remote-backup-destination/test`: 202 with job_id.
- `GET /api/v1/admin/remote-backup-destination/remotes`: configured rclone names.

Guided rclone connection setup also requires `panel.settings`. A setup lives in
memory for 15 minutes and belongs to the login session that started it; any other
session, an expired setup, or one lost to a restart returns 404.

- `POST /api/v1/admin/rclone-setups`: 201 with the setup. The body names `provider`
  (`drive`, `dropbox`, `onedrive`, `mega` or `s3`), `name`, and that provider's fields:
  `client_id` and `client_secret`; `user` and `password`; or `s3_provider`, `endpoint`,
  `region`, `access_key_id`, `secret_access_key` and `session_token`. A taken name
  returns 409 `name_taken`.
- `GET /api/v1/admin/rclone-setups/{id}`: the setup's `step` (`authorize`,
  `choose_drive` or `ready`), `callback_url`, `authorization_url` while authorizing,
  `drives` while choosing, and `expires_at`. Credentials and tokens are never returned.
- `POST /api/v1/admin/rclone-setups/{id}/advance`: either the provider's redirect as
  `state`, `code` and `error`, or the chosen OneDrive `drive_id`. A refused or failed
  authorization returns 409 `invalid_state` and opens a new authorization request.
- `DELETE /api/v1/admin/rclone-setups/{id}`: 204, discarding the setup.
- `POST /api/v1/admin/rclone-setups/{id}/finish`: 202 with the `remote_create` job that
  adds the remote to the rclone configuration. It returns 409 `job_in_progress` while
  a remote transfer runs.

Instance PATCH accepts `remote_backup_enabled`, `remote_keep_cold`,
`remote_keep_hot`, and `remote_keep_snapshots`, requiring `backups.restore`.

- `POST /api/v1/instances/{id}/backups/{bid}/remote-copy` queues a durable copy.
- `GET /api/v1/instances/{id}/remote-copies` returns cursor-paginated items and summary.
- `GET /api/v1/instances/{id}/remote-copies/{copy_id}` returns one copy.
- `POST /api/v1/instances/{id}/remote-copies/{copy_id}/retry` retries a failed or cancelled copy.
- `POST /api/v1/instances/{id}/remote-copies/{copy_id}/cancel` stops pending work.

Upload controls require `backups.create`; reads require `backups.list`.
Invisible instances return 404. Upload/retry returns 202 with `copy_id`, `status`,
and nullable `job_id`. Location identifies the durable copy resource: a queued
copy may not have an attempt job yet. Each attempt uses the existing jobs API.

Copy states are pending, uploading, retry_wait, succeeded, failed, cancelled, and
pruned. Cleanup status is separate. Secrets, source paths, and provider object IDs
are never exposed. See [remote backup operations](remote-backups.md).

## Discord bot

Bot administration requires `panel.settings`; everyone else gets `404`.

- `GET /api/v1/admin/discord`: `configured` (a token is stored), `enabled`,
  `links`, `admin_ids`, `timezone`, `status` (`state` is `disabled`, `connecting`, `connected` or `failed`,
  with `bot_name`, `application_id` and `error`), and `invite_url` once the bot
  has connected. The token is never returned.
- `PUT /api/v1/admin/discord`: the whole document — optional write-only `token`,
  `enabled`, `links`, each with `guild_id`, `channel_id` (empty for every
  channel), `allow_start` and `instance_ids`, `admin_ids` (Discord user or role IDs
  that may use `/restart`, `/stop`, `/backup`, `/update` and `/shutdown`) and `timezone` (the IANA zone typed times are read in,
  UTC when empty). An absent or empty token keeps the
  stored one. IDs are 17 to 20 digits, a Discord server and channel may appear in
  one link only, and enabling the bot needs a token. The links sent replace every
  stored link, and the bot reconnects.
- `DELETE /api/v1/admin/discord`: removes the token and every link. Returns 204.

Each save and delete writes an audit entry with action `panel.settings` and
operation `discord_update` or `discord_delete`. A command from Discord appears in
the audit log by `Discord: <name> (<user id>)`, under the action the panel records for the
same change: `instances.start`, `instances.restart`, `instances.stop`,
`instances.backups.create`, `instances.update_test.start`, `instances.update_test.apply`, or
`instances.delete` for a discarded update test copy. A restart, stop or backup that
waits for players is a `chain` job.

## Planned power cuts

Planned power cuts require `schedules.global`; everyone else gets `404`. Every running
server is stopped from `stop_at`, which leaves room for a stop that takes the whole
`game.stop_timeout` (the timeout plus 40 seconds, in whole minutes, at least two). Players
are warned at `warn_at`, five minutes before `stop_at`.

- `GET /api/v1/admin/shutdowns`: the upcoming power cuts, soonest first, each with `id`,
  `power_off_at`, `stop_at`, `warn_at`, `created_by_username` (a panel account, or null) and
  `created_by_name` (a Discord admin, or empty).
- `POST /api/v1/admin/shutdowns`: `{"local_time": "2026-10-09T14:00", "timezone":
"Europe/Kyiv"}`. `local_time` is read on the clock of `timezone`; `15:04` alone means
  the next such time. A time that has passed is `422` on `local_time`. Returns 201.
- `DELETE /api/v1/admin/shutdowns/{id}`: returns 204, or 404.

Audit actions are `shutdowns.create` and `shutdowns.delete`. Each stop is audited as
`instances.stop` by `Planned shutdown`.
