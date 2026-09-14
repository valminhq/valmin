# Install Valmin

[Documentation](README.md) / Install Valmin

These instructions build images from this checkout and run the supplied Docker
Compose deployment. Go and Node.js run inside the build containers.

## Requirements

- Linux x86-64 with Docker Engine, Docker Compose v2, Git, and Make.
- Permission to run Docker commands and `sudo` for the initial host setup.
- Storage for game installations, worlds, and backups. Valmin refuses to start
  with less than 2 GiB free by default; that is a startup floor, not a storage budget.
- Memory for the host and each server. The default server limit is 4096 MiB.
- A hostname pointing to the host, with TCP ports 80 and 443 available for Caddy.
- Outbound access to container registries, Steam, and Thunderstore.

The supplied deployment uses a local Docker daemon and UID/GID `10000:10000`.
Run it on a host you control. The socket proxy limits Docker API access, but the
panel's ability to create containers still gives it root-equivalent access to the host.

## Build and configure

```sh
git clone https://github.com/valminhq/valmin.git
cd valmin
make panel-image game-image
cp deploy/.env.example deploy/.env
```

Edit `deploy/.env`. Use these values for the locally built images and replace the
hostname with yours:

```dotenv
VALMIN_DOMAIN=valmin.example.com
VALMIN_HOST_DATA_ROOT=/srv/valmin
VALMIN_IMAGE=valmin/valmind:dev
VALMIN_GAME_IMAGE=valmin/valheim:dev
VALMIN_TLS=
```

The registry version tags in `.env.example` are examples. This setup uses the
images you just built. For registry deployments, use published references and pin
them by digest for repeatable upgrades.

For a LAN hostname, set `VALMIN_TLS='tls internal'` and trust Caddy's local CA on
each client. Keep `.env` valid shell syntax: `prepare-host.sh` also reads it.

## Prepare the host and start

```sh
sudo ./deploy/prepare-host.sh /srv/valmin
cd deploy
docker compose config --quiet
docker compose up -d
docker compose ps
docker compose logs -f valmind
```

If you changed `VALMIN_HOST_DATA_ROOT`, pass that same absolute path to the setup
script. It creates the data directory and host account, checks ownership, and
obtains the game and SteamCMD images. It refuses to change an existing directory's
ownership.

Open `https://your-hostname`. Enter the first-run token from the `valmind` logs and
create your administrator account. The token expires after 15 minutes. Until an
administrator exists, `docker compose restart valmind` prints a new token.

Caddy serves the panel over HTTPS. Neither panel port 8080 nor the Docker proxy is
published on the host. Compose reserves `10.89.13.0/24` and `10.89.14.0/24`; if
those overlap your network, adjust the subnets, static addresses, and trusted
proxy address together in [compose.yaml](../deploy/compose.yaml).

## Open game ports

Game traffic goes directly to the host, separately from Caddy. Valmin publishes two
UDP ports per server: its base port and the following port. Allocation starts at
2456 and advances by five by default: `2456–2457`, `2461–2462`, `2466–2467`.

Check the assigned port in the panel. For direct connections, allow that pair
through the firewall and forward it on your router if the host is behind NAT.
Valmin skips occupied blocks, so do not assume every server gets the next pair.

Next: [create your first server](usage.md#create-a-server).
