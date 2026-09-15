# Troubleshooting

[Documentation](README.md) / Troubleshooting

## Inspect the deployment

Run from `deploy/` on the server:

```sh
docker compose ps
docker compose logs --tail=100 valmind caddy docker-proxy
docker compose exec valmind valmind version
docker compose exec valmind valmind healthcheck
```

The last two commands require a running panel container. If it is restarting,
read its logs first. Caddy waits for the panel's healthcheck, so a panel startup
failure can also leave HTTPS unavailable. Remove setup tokens and other credentials
before sharing logs.

## Registry says denied or an image is missing

A registry `denied` response can mean the image is private or the reference is
unavailable. It does not establish that your Docker installation is broken.

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

## Data directory ownership

The panel and game containers use UID/GID `10000:10000`. Inspect the configured
host directory:

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
| Login succeeds but immediately returns to login | Use HTTPS. Browsers reject the panel's Secure cookies on non-localhost HTTP.                                                                         |
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
mod versions. Wait for the panel to report the server ready, then check its game
logs. The panel's HTTPS port is unrelated to the game's UDP ports.

The console displays logs only; this build does not send commands to the game.
