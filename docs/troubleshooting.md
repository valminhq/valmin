# Troubleshooting

[Documentation](README.md) / Troubleshooting

## Check the diagnostics page

Administrators find **Diagnostics** in the panel header. It reports, on one page, most of
what the sections below ask you to check by hand:

- Whether Docker answers, and the API version it negotiated.
- Whether the game and SteamCMD images are present on the host.
- Free space on the data root, its filesystem type, and the account the daemon runs as.
- Whether the host data path and game network checks passed at startup.
- Whether Thunderstore and Hexium are enabled and reachable, when each last synced,
  and whether its latest refresh failed or its catalogue is stale.
- When Steam last reported a build.
- Whether the external URL lets a browser store the panel's session cookies.
- Whether each running server publishes the UDP ports it was allocated.

The server table includes the last reported free space, container restart count, and
last exit code, including out-of-memory termination. A server whose container runs another
image than `game.image` says so under its name; recreate the container to move it. The
summary line counts the servers with problems alongside the checks. A failed mod read or container
inspection says **Could not check** and shows its error. Server links open the overview,
backups, or mods. **Failed operations** opens job details without leaving diagnostics:
the error and its code, who started the job, how long it ran, its outcome, and a link
to the screen to check next when the operation has one. **No error details recorded.**
appears only for a failed job with no error and no message.

**Show problems only** hides passing checks and servers without reported problems.
Missing measurements remain visible for running servers. The latest registry refresh
outcome is recorded from the first refresh after upgrading.

Every entry states where its answer came from and when it was measured: `startup` for a
check the panel recorded when it started, `job` for one the deep checks recorded. Each live
probe waits at most five seconds, so a Docker engine or registry that hangs shows as failing
rather than stalling the page. An entry marked
`unknown` was never measured. Read it as a missing answer, not as a pass.

**Run deep checks** repeats the host data path, game network, and Steam checks. Each starts
a temporary container, so it runs as a job instead of during a page load. The job succeeds
even when a check fails, so no failed-job alert fires; the result is on this page.

Published ports are not proof that players can connect. The page compares the ports a
server was allocated against the ports Docker publishes for it, inside this host. It cannot
test your router or firewall; see
[game server runs but players cannot connect](#game-server-runs-but-players-cannot-connect).

### Support bundle

**Download support bundle** produces a zip file for a bug report. It contains the
diagnostics report, the applied schema history, and the daemon's settings.

The bundle is built to be posted unedited. It does not collect secrets, the database DSN,
the master key path, absolute filesystem paths, the external URL's host, world names,
player identifiers, or any console output. Facts derived from those paths, such as free
space, filesystem type, and the UID the daemon runs as, are reported in their place.

Servers are named `server-1`, `server-2` and so on, by their position in the report; their
ids, which are random, stay. Image references other than the shipped `valmin/valheim` and
`steamcmd/steamcmd`, and a Thunderstore URL other than `https://thunderstore.io`, read
`custom`, followed by the image digest when the reference has one. A shipped image keeps
a `dev`, `latest` or version tag; any other tag reads `custom`.

It also omits the verbatim error text from a failed check, because those messages name
filesystem paths. That text is on the diagnostics page. Quote the part you are willing to
share.

When you cannot sign in, produce the same bundle from the command line. Use `-T` so the
zip is not written through a terminal:

```sh
docker compose exec -T valmind valmind diagnose > valmin-support.zip
```

If the panel container will not stay running, use a one-off container instead:

```sh
docker compose run --rm --no-deps -T valmind valmind diagnose > valmin-support.zip
```

Neither form needs a session, and neither opens the panel's database, so the checks
recorded at startup and the last Steam result report `unknown`. The configuration, data
root, and Thunderstore checks still run. The Docker checks run when the socket answers and
report `unknown` when it does not, which is itself the answer you are looking for on a host
where the panel will not start. The whole run stops after a minute.

`valmind diagnose` reads the same configuration as the daemon. When that configuration
is invalid, it prints the same error the daemon does and writes no bundle; fix the
setting it names first.

## Inspect the deployment

Run from `deploy/` on the server:

```sh
docker compose ps
docker compose logs --tail=100 valmind caddy docker-proxy
docker compose exec valmind valmind version
docker compose exec valmind valmind healthcheck
```

The last two commands require a running panel container. If it is restarting,
read its logs first. `valmind diagnose` writes the same facts as a
[support bundle](#support-bundle). Caddy waits for the panel's healthcheck, so a panel startup
failure can also leave HTTPS unavailable. Remove setup tokens and other credentials
before sharing logs.

## Registry says denied or an image is missing

A registry `denied` response can mean the image is private or the reference is
unavailable. It does not establish that your Docker installation is broken. If the panel
is running, [Diagnostics](#check-the-diagnostics-page) reports which of the two configured
images are present on the host.

For an installation from source, build both images from the repository root:

```sh
make panel-image game-image
```

Use their local names in `deploy/.env`:

```dotenv
VALMIN_IMAGE=valmin/valmind:dev
VALMIN_GAME_IMAGE=valmin/valheim:dev
```

Then run `sudo ./deploy/prepare-host.sh /srv/valmin` from the repository root,
substituting your configured data directory. The script obtains missing game and
SteamCMD images; it does not build images or obtain the panel image.

For the published images, `denied` from `ghcr.io` usually means the release tag does
not exist; check the tag against the [releases page](https://github.com/valminhq/valmin/releases).
`toomanyrequests` is an anonymous pull limit, common from shared or CGNAT addresses:
run `sudo docker login` for that registry (`docker.io` for SteamCMD, Caddy and the
socket proxy) and run the same command again.

If you intentionally use a private registry, authenticate with that registry and
verify the exact image reference. The script runs Docker as root, whose registry
credentials and Docker context can differ from your login user's. This deployment
expects both to reach the same local Docker Engine.

## The install script stops

The install script checks the host before it downloads or writes most things. After
any of these errors, fix the cause and run the same command again; it continues an
install that has not finished.

| Message                                         | Fix                                                                                                                                         |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `cannot reach github.com` / `cannot download`   | Check the host's DNS and internet access. Behind an HTTP proxy, see [quick install](installation.md#quick-install).                         |
| `no published release yet`                      | Pass `--version` with a tag from the [releases page](https://github.com/valminhq/valmin/releases).                                          |
| `cannot pull ghcr.io/…`                         | See [registry errors](#registry-says-denied-or-an-image-is-missing).                                                                        |
| `port 80 is already in use` (or 443)            | Stop the other web server. For a LAN address, you can pass `--http-port 8081 --https-port 8443` instead; a public certificate needs ports 80 and 443 reachable from the internet. |
| `… is already routed on this host`              | A LAN, VPN or Docker network uses that range. Pass `--subnet-prefix` with a free pair, such as `172.30`.                                    |
| `a network named valmin-games already exists`   | Left by `make dev`. Remove it with `docker network rm valmin-games`.                                                                        |
| `… is not owned by 10000:10000`                  | The data directory was created by another account. Remove it if it is empty, or pass another `--data-root`. See [ownership](#data-directory-ownership). |
| `the default route goes through VPN interface`  | Pass `--domain` with the address browsers will use.                                                                                         |
| `cannot detect this host's address`             | Pass `--domain`.                                                                                                                            |
| `… does not resolve`                            | Create the DNS record first, or pass `--local` for a hostname only your LAN resolves.                                                       |
| `… is already installed in /opt/valmin`         | An earlier run finished. To upgrade, run it with `--upgrade`; see [update the panel](operations.md#update-the-panel-or-game).              |
| `no install made by this script`                | `--upgrade` found no finished install in `--dir`. Pass the `--dir` you installed to, or upgrade [by hand](operations.md#update-the-panel-or-game). |
| `… is older than the running …`                 | `--upgrade` never goes back. To roll back, follow [update the panel](operations.md#update-the-panel-or-game).                              |
| `another Valmin stack` / `already holds a Valmin database` | An installation from source uses that stack or data directory. Upgrade it instead, or install with another `--data-root`.     |
| `Docker runs with iptables disabled` (warning)  | Remove `"iptables": false` from `/etc/docker/daemon.json`; without it, published ports and container downloads do not work.                 |

If the stack does not start, the installer prints where to look:
`docker compose logs valmind caddy` from `/opt/valmin`. Fix the cause and run the
installer again.

## Containers cannot reach Steam or Thunderstore

The host can download but server installs or mod downloads fail or stall. Run
**Diagnostics → Run deep checks**: it asks Steam for the current build through
SteamCMD and checks the mod registries from the panel's own container.

- **DNS.** When the host resolves only through a resolver on the host itself, such as
  dnsmasq on `127.0.0.1`, containers cannot use it, and Docker falls back to Google's
  public DNS, which some networks block. Set `"dns": ["<your router or DNS server>"]`
  in `/etc/docker/daemon.json` and restart Docker.
- **MTU.** On links with an MTU below 1500, such as some VPS networks and PPPoE, small
  requests succeed while large downloads stall. Set `"mtu"` in `daemon.json` to the
  host interface's MTU (`ip link`), and add the same value to each network in
  `compose.yaml` as `driver_opts: {com.docker.network.driver.mtu: "1450"}`.

## The mod catalogue is empty or out of date

The panel searches its own copy of each registry's index and refreshes it on the
interval set by `VALMIN_THUNDERSTORE_SYNC_INTERVAL`, one hour by default. A fresh
install has no catalogue until that first refresh finishes, and the **Mods** screen
says which registry it is still waiting for.

Diagnostics shows the latest refresh result for each registry separately. A run that
fails for one registry still records the other, and the panel keeps serving the last
index it stored. A catalogue older than two refresh intervals is marked out of date.

If one registry is permanently missing from search, check it is enabled and that its
`base_url` is reachable from the panel container. The daemon refuses to start on an
absolute-URL failure, so a running panel with a missing registry points at a network
or upstream problem rather than a typo.

Hexium serves no cache validators, so it re-downloads its whole index on every
refresh by design. That is expected traffic of a few megabytes per interval and not
a fault; set `VALMIN_HEXIUM_ENABLED=false` if it is unwanted.

## A profile code is not found

Pasting an r2modman or Gale code under **New from template** answers that no profile has
the code when neither Thunderstore nor Hexium serves it. Profile codes expire, so ask for a
new one. If the answer says the registry could not be reached, check the registry on the
diagnostics page and try again. Profile codes use the registries' experimental endpoints,
which can change without notice.

## Data directory ownership

The panel and game containers use UID/GID `10000:10000`.
[Diagnostics](#check-the-diagnostics-page) reports the account the daemon itself runs as.
Inspect the configured host directory:

```sh
stat -c '%u:%g %a %n' /srv/valmin
```

For a newly created directory, `prepare-host.sh` sets ownership to `10000:10000`
and mode `2775`. It refuses to change an existing directory's ownership.

If the directory belongs to this Valmin installation and the reported owner is
`10000:0`, fix the top-level group and inheritance:

```sh
sudo chgrp 10000 /srv/valmin
sudo chmod 2775 /srv/valmin
```

Rerun `prepare-host.sh` with that path. These commands do not change files inside
the directory. If existing saves have different ownership, stop their server and
inspect those files before correcting them. Do not recursively change ownership
of an unrelated directory to make the check pass.

## Host data path check fails

`VALMIN_DATA_ROOT` is the path inside the panel container. `VALMIN_DATA_HOST_ROOT`
is the same directory as seen by Docker on the host. Compose derives the latter
from `VALMIN_HOST_DATA_ROOT` in `.env`.

For example, a bind mount `/mnt/games/valmin:/srv/valmin` needs `/mnt/games/valmin`
as the host root. Verify the configured game image exists too: the path check
uses that image to read a temporary file through Docker. See [configuration](configuration.md#compose-and-data-paths).

This check runs at every startup, and the panel does not start when it fails. On a running
panel, [Diagnostics](#check-the-diagnostics-page) shows when it last passed, and
**Run deep checks** repeats it.

## HTTPS and certificate errors

Use the exact address configured for the panel: for example,
`https://192.168.1.100`, on port 443. Do not use `https://192.168.1.100:8080`.
For LAN access, set `VALMIN_TLS='tls internal'` and recreate changed services with
`docker compose up -d`.

Different errors need different fixes:

| Symptom                                         | Next step                                                                                                                                            |
| ----------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| Connection refused or timed out                 | Check `docker compose ps`, the host firewall, the IP, and WiFi client isolation.                                                                     |
| Unknown issuer or untrusted certificate         | [Trust Caddy's local CA](installation.md#trust-the-local-certificate) on the client.                                                                 |
| Certificate belongs to a different address      | Make the browser URL match `VALMIN_DOMAIN`, then apply any configuration changes.                                                                    |
| Generic “Secure Connection Failed”              | Record the browser's exact error code. Check Caddy logs; a TLS handshake failure is not necessarily a trust problem.                                 |
| Login succeeds but immediately returns to login | Use HTTPS. Browsers reject the panel's Secure cookies on non-localhost HTTP. [Diagnostics](#check-the-diagnostics-page) warns about this.            |
| `403 origin_rejected`                           | Match the configured external URL to the browser's scheme, address, and port. See [custom HTTPS ports](configuration.md#use-a-different-https-port). |
| Public domain: Caddy logs ACME or challenge errors | Check that the domain's A record, and any AAAA record, point at this host; the CA tries IPv6 first. Forward port 80 or 443 to the host. Behind CGNAT, use a DNS challenge. |
| Public domain works outside the LAN but not inside | The router lacks NAT loopback. Point the name at the server's LAN IP in local DNS or the hosts file.                                |

Caddy logging `certificate obtained successfully` confirms issuance, not that your
client trusts the certificate. “Certificate installed properly in linux trusts”
refers to the Caddy container. The `certutil` and Java trust messages do not mean
certificate issuance failed.

From a client with curl, check the response and certificate error:

```sh
curl -v https://192.168.1.100/healthz
```

Replace the IP with your configured address. With the exported CA file on that
client, verify explicitly:

```sh
curl --cacert valmin-root.crt https://192.168.1.100/healthz
```

A working connection returns `ok`. If this works but the browser fails, check the
browser's CA trust settings. If TLS itself fails, capture the curl error and the
browser error code before changing certificates.

## Setup token is rejected

The token expires after 15 minutes and changes on every restart until the first
administrator exists. From `deploy/`:

```sh
docker compose restart valmind
docker compose logs --tail=80 valmind
```

Use the newest token. Once setup is complete, restarting does not reopen it.

## Reset an account password

While you are signed in, change your own password from the account menu instead; see
[change your password](usage.md#change-your-password). The command below is for an account
you cannot sign in to.

From `deploy/`:

```sh
docker compose exec valmind valmind admin reset --username YOUR_USERNAME
```

The command prints a new password and revokes that user's sessions. If the panel
cannot stay running, the recovery command can run in a one-off container:

```sh
docker compose run --rm --no-deps valmind valmind admin reset --username YOUR_USERNAME
```

Run beside a panel that is up, the command uses the panel's database as it is and changes no
schema, so run it with the image the panel runs. Recovery needs the configured database and
writable data directory, but does not need a working Docker proxy connection from the panel.

## Panel does not start after an upgrade or restore

The panel exits at startup and the log shows one of these errors.

`migrations: database was written by a newer valmind: ...` means this build is older
than the database. Run the newer build again, or
[roll back](operations.md#update-the-panel-or-game) by restoring the
`backups/panel-pre-*.db` copy taken before the upgrade.

`back up the database before migrating: ...` means the panel could not copy the
database before a migration. Check free space and that the panel account can
write `backups/` under the data root.

`master key: master key file is missing at ...` means `secret.key` is gone but the
database already uses a key. The panel does not create a new one.

`master key: the master key does not match this database; ...` means `secret.key`,
`VALMIN_MASTER_KEY`, or `VALMIN_MASTER_KEY_FILE` holds a different key from the one
the database was set up with.

For both key errors, restore the `secret.key` backed up with this `panel.db`. If the
key is lost for good, see
[Recover from a lost master key](operations.md#recover-from-a-lost-master-key).

## The data disk is full

When the data root has less free space than `data.free_space_floor_bytes` (2 GiB by
default), the panel still starts, and its home page shows the low-disk condition. Mod
installs, game updates, new servers, clones, backups, restores, world imports, world
tools and setup saves and restores are refused with `insufficient_disk` until there is
room again. Starting and stopping servers, editing configs and deleting backups, worlds,
setups and servers still work, so delete what you no longer need from the panel. The
refusal lifts as soon as enough space is free, without a restart.

## A game server does not start or mods do not load

When a start fails, or a running server's mods did not load, the server's **Console** card
explains what the panel observed above the log. Each explanation quotes the log line it rests on,
lists what to do, and links to the screen where you fix it. It covers:

- The kernel stopped the server for running out of memory.
- A mod failed to load. Mods skipped because of it are listed under it, not as separate failures.
- The server exited before it was ready. The first error the mod loader recorded is quoted,
  otherwise the last line the server printed.
- The start timed out while the server was still running. The first start after a game update
  may still be converting the world.
- A modded server started without its mod loader running.

Anything else shows up only in the console itself. The same explanation is available from
`GET /api/v1/instances/{id}/diagnosis`.

A start that fails with Docker's "port is already allocated" names the UDP port another
program holds. When choosing ports for a new server, the panel skips ports its own servers
and running containers use. It cannot see a program on the host outside Docker, or a stopped
container that publishes the port once started. Stop that program, then start the server
again.

## A game install or update fails on a branch

`Failed to set beta '<branch>'` in the job's error means Steam refused the branch: its
password is wrong, or the branch does not exist right now. A test branch such as
`public-test` exists only while the game's developers run a test, and they can change its
password. Enter the current name and password under **Game version** on the **Maintenance**
page, or pick a listed branch.

`steam installed branch public ... when <branch> was asked for` means SteamCMD reported
success but installed another branch. Nothing was published; run the update again.

## Game server runs but players cannot connect

Check the assigned UDP port pair, firewall/NAT rules, server password, client
mod versions, and that players chose the server's branch in Steam (Valheim's
**Properties**, **Betas**). [Diagnostics](#check-the-diagnostics-page) confirms only the first hop: that
Docker publishes the ports the server was allocated. Forwarding beyond this host is not
something the panel can test. Wait for the panel to report the server ready, then check its game
logs. The panel's HTTPS port is unrelated to the game's UDP ports.

## Console commands are unavailable or fail

The note under the console input says why it is disabled: the server lacks the
`Tristan-ValheimRcon` mod, your role cannot send commands, or the server is not
running. See [send server commands](usage.md#send-server-commands). When a command
is refused:

- **Some values are invalid**: the command is empty, over 1,024 bytes, or not
  allowed for your role.
- **This action is unavailable in the server's current state**: the server is not
  running. Start it and wait until it is ready.
- **Too many requests**: the server's command limit was reached. Wait a minute.
- **Valmin is temporarily unavailable**: the panel could not reach or sign in to the
  mod. Check that `BepInEx/config/org.tristan.rcon.cfg` has a `Port` under
  `[1. Rcon]` and that the game network check on
  [Diagnostics](#check-the-diagnostics-page) passes. The daemon log entry with the
  error's request ID names the cause.
