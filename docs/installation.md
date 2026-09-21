# Install Valmin

[Documentation](README.md) / Install Valmin

This guide builds images from the checkout and runs the supplied Docker Compose
stack. Run the commands on the machine that will host your servers. Go and Node.js
run inside the build containers; you do not need to install them on the host.

## Requirements

- Linux x86-64 with Docker Engine, Docker Compose v2, Git, and Make.
- Permission to run Docker commands and `sudo` for the initial host setup.
- Storage for installations, saves, and backups. The default startup check requires
  2 GiB free; allow additional space for each server and its backup history.
- Memory for the host and each server. The default per-server limit is 4096 MiB.
- TCP ports 80 and 443 available on the host, or [custom ports](configuration.md#use-a-different-https-port).
- Outbound access to container registries, Steam, and the mod registries
  (`thunderstore.io` and `valheim.hexium.gg`; mod downloads come from their CDNs).

Check Docker before proceeding:

```sh
docker info
docker compose version
```

The supplied deployment uses the host's local Docker daemon and UID/GID
`10000:10000`. The socket proxy restricts Docker API access, but creating containers
still gives the panel root-equivalent access to that host.

## Build the images

```sh
git clone https://github.com/valminhq/valmin.git
cd valmin
make panel-image game-image
cp deploy/.env.example deploy/.env
```

Copy the example only for a new installation; it would overwrite an existing `.env`.
Keep these image settings for the images you just built:

```dotenv
VALMIN_IMAGE=valmin/valmind:dev
VALMIN_GAME_IMAGE=valmin/valheim:dev
VALMIN_HOST_DATA_ROOT=/srv/valmin
```

No registry login is needed for these two local images. The game image contains
runtime libraries; SteamCMD downloads Valheim when you create a server.

For registry deployments, replace both image references with available images,
preferably pinned by digest. The current [release workflow](../.github/workflows/release.yml)
publishes the panel image; it does not publish the game runtime image. Do not assume
that a matching `ghcr.io/valminhq/valheim` release tag exists.

## Choose the panel address

Edit `deploy/.env` for one of the following setups. Keep shell values containing
spaces in quotes because `prepare-host.sh` also reads this file.

### Local WiFi or LAN

Use the server's LAN IP, replacing the example with your own:

```dotenv
VALMIN_DOMAIN=192.168.1.100
VALMIN_TLS='tls internal'
```

You will browse to **https://192.168.1.100**, on port **443**. Reserve the server's
IP in your router's DHCP settings so it does not change. No router port forwarding
is needed for clients on the same LAN. Allow HTTPS through the host firewall;
guest WiFi or client isolation can block connections between devices.

A local hostname also works if each device resolves it to the server's IP through
local DNS or a hosts-file entry. Use that hostname in `VALMIN_DOMAIN` and in the
browser. Do not include `https://`, a port, or a path in `VALMIN_DOMAIN`.

Caddy issues a local certificate. After startup, [trust its CA](#trust-the-local-certificate)
on the devices you use to access the panel.

### Public domain

If you control a domain and want Caddy to obtain a publicly trusted certificate:

```dotenv
VALMIN_DOMAIN=valmin.example.com
VALMIN_TLS=
```

Point the domain's DNS records to your server's public address. With the default
Caddy configuration, certificate validation needs inbound access on port 80 or 443. Behind a router, forward the required ports to the server. This can also make
the panel reachable from the internet.

A domain can use a publicly trusted certificate while the panel stays LAN-only,
using DNS validation. That requires a DNS-provider plugin and credentials in
Caddy; it is not configured by this Compose file. See
[Caddy's certificate challenge documentation](https://caddyserver.com/docs/automatic-https#dns-challenge).

## Prepare the data directory

From the repository root, after saving `deploy/.env`:

```sh
sudo ./deploy/prepare-host.sh /srv/valmin
```

If you changed `VALMIN_HOST_DATA_ROOT`, pass the same absolute path here. The script
argument defaults to `/srv/valmin`; it does not take the directory argument from `.env`.

The script creates the host account and directory, checks ownership, and obtains
missing game and SteamCMD images. It does not start the panel. Let it create a new
directory instead of creating one as root beforehand.

You normally run this once. It can be rerun to check the host or obtain newly
configured images. Changing only the browser address or TLS setting does not
require rerunning it. For an existing directory with the wrong owner, see
[ownership errors](troubleshooting.md#data-directory-ownership).

## Start the panel and create an administrator

```sh
cd deploy
docker compose config --quiet
docker compose up -d
docker compose ps
docker compose logs -f valmind
```

The panel must become healthy before Caddy starts. The logs should show the
first-run setup token. Press Ctrl+C to leave the log viewer; the containers keep running.

Open the HTTPS address you configured. For a LAN deployment, complete the certificate
trust step below. Enter the setup token and create an administrator account with a
password of at least eight characters.

The token expires after 15 minutes. Until an administrator exists,
`docker compose restart valmind` prints a new token. Use the newest one. After setup,
use [password recovery](troubleshooting.md#reset-an-account-password) if needed.

### Trust the local certificate

For `tls internal`, export Caddy's public root certificate on the server, from `deploy/`:

```sh
docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt ./valmin-root.crt
```

Copy `valmin-root.crt` to each client device. Import it as a trusted certificate
authority in the operating system or browser. In Firefox, open Settings, search
for certificates, and use **View Certificates → Authorities → Import**. Enable
trust for identifying websites when prompted. A browser-specific import covers
that browser; other browsers may need their own trust configuration. See
[Firefox certificate guidance](https://support.mozilla.org/en-US/kb/what-does-your-connection-is-not-secure-mean).

Only copy `root.crt`, never Caddy's private key. The Caddy log message about
installing a certificate refers to its container, not your other devices.
Caddy cannot install trust remotely. See [Caddy's local HTTPS documentation](https://caddyserver.com/docs/automatic-https#local-https).

This is a one-time step while Caddy retains the same CA. Keep the `caddy_data`
volume across upgrades. Replacing that volume creates a new CA and requires
trusting the new root. A generic “Secure Connection Failed” message can also mean
a handshake failure; see [HTTPS troubleshooting](troubleshooting.md#https-and-certificate-errors).

## Check network access

| Traffic          | Default port             | Address to use                                     |
| ---------------- | ------------------------ | -------------------------------------------------- |
| Panel HTTPS      | TCP 443                  | `https://YOUR_IP_OR_HOSTNAME`                      |
| HTTP redirect    | TCP 80                   | Redirects to HTTPS.                                |
| HTTP/3, optional | UDP 443                  | Caddy's HTTPS transport. TCP HTTPS also works.     |
| Valheim game     | Two UDP ports per server | Host address and the base port shown in the panel. |

The daemon's port 8080 and Docker proxy port 2375 are internal. Do not use port
8080 in the browser. Game traffic goes directly to game containers, not through Caddy.

Valmin allocates port pairs starting at `2456–2457`, advancing by five:
`2461–2462`, `2466–2467`, and so on. Occupied blocks are skipped. Allow the assigned
pair through the host firewall. For direct game connections from outside your LAN,
forward that pair on the router as well.

Compose reserves `10.89.13.0/24` and `10.89.14.0/24`. If those overlap your network,
adjust the subnets, static addresses, and trusted proxy address together in
[compose.yaml](../deploy/compose.yaml).

Next: [create your first server](usage.md#create-a-server) or check
[restart behavior after a reboot](operations.md#restart-after-a-reboot).
