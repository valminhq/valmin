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
last exit code, including out-of-memory termination. A failed mod read or container
inspection says **Could not check** and shows its error. Server links open the overview,
backups, or mods. **Failed operations** opens job details without leaving diagnostics.

**Show problems only** hides passing checks and servers without reported problems.
Missing measurements remain visible for running servers. The latest registry refresh
outcome is recorded from the first refresh after upgrading.

Every entry states where its answer came from and when it was measured. An entry marked
`unknown` was never measured. Read it as a missing answer, not as a pass.

**Run deep checks** repeats the host data path, game network, and Steam checks. Each starts
a temporary container, so it runs as a job instead of during a page load.

Published ports are not proof that players can connect. The page compares the ports a
server was allocated against the ports Docker publishes for it, inside this host. It cannot
test your router or firewall; see
[game server runs but players cannot connect](#game-server-runs-but-players-cannot-connect).

### Support bundle

**Download support bundle** produces a zip file for a bug report. It contains the
diagnostics report, the applied schema history, and the daemon's settings.

The bundle is built to be posted unedited. It does not collect secrets, the database DSN,
the master key path, absolute filesystem paths, world names, player identifiers, or any
console output. Facts derived from those paths, such as free space, filesystem type, and
the UID the daemon runs as, are reported in their place.

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
where the panel will not start.

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

If you intentionally use a private registry, authenticate with that registry and
verify the exact image reference. The script runs Docker as root, whose registry
credentials and Docker context can differ from your login user's. This deployment
expects both to reach the same local Docker Engine.

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

From `deploy/`:

```sh
docker compose exec valmind valmind admin reset --username YOUR_USERNAME
```

The command prints a new password and revokes that user's sessions. If the panel
cannot stay running, the recovery command can run in a one-off container:

```sh
docker compose run --rm --no-deps valmind valmind admin reset --username YOUR_USERNAME
```

Recovery needs the configured database and writable data directory, but does not
need a working Docker proxy connection from the panel.

## Game server runs but players cannot connect

Check the assigned UDP port pair, firewall/NAT rules, server password, and client
mod versions. [Diagnostics](#check-the-diagnostics-page) confirms only the first hop: that
Docker publishes the ports the server was allocated. Forwarding beyond this host is not
something the panel can test. Wait for the panel to report the server ready, then check its game
logs. The panel's HTTPS port is unrelated to the game's UDP ports.

The console displays logs only; this build does not send commands to the game.
