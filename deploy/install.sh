#!/bin/sh
# Installs or upgrades Valmin from a published release: fetches the deployment files, pins the
# panel and game images by digest, writes .env, prepares the host and starts the stack.
#
#   curl -fsSL https://github.com/valminhq/valmin/releases/latest/download/install.sh | sudo sh
#   curl -fsSL https://github.com/valminhq/valmin/releases/latest/download/install.sh | sudo sh -s -- --upgrade
#   curl -fsSL https://github.com/valminhq/valmin/releases/latest/download/install.sh | sudo sh -s -- --restore /root/export.tar.gz
#
# The whole script is one brace group, so a truncated download runs nothing.
set -eu
{

REPO=valminhq/valmin
REGISTRY=ghcr.io/valminhq
DOCS=https://github.com/$REPO/blob/main/docs

DIR=/opt/valmin
DATA_ROOT=/srv/valmin
DOMAIN=
LOCAL=
VERSION=
HTTP_PORT=80
HTTPS_PORT=443
SUBNET_PREFIX=10.89
UPGRADE=
RESTORE=
configured=

die() {
	echo "install: $*" >&2
	exit 1
}

usage() {
	cat <<EOF
Usage: install.sh [options]
       install.sh --upgrade [--dir PATH] [--version TAG]

  --domain NAME         Browser hostname or LAN IP. Default: this host's LAN address.
  --local               Use Caddy's local CA for a hostname that is not public.
  --data-root PATH      Data directory on this host. Default: $DATA_ROOT
  --dir PATH            Where the Compose files go. Default: $DIR
  --version TAG         Release to install, such as v1.7.0. Default: the latest release.
  --http-port PORT      Host port for HTTP. Default: 80
  --https-port PORT     Host port for HTTPS. Default: 443
  --subnet-prefix A.B   First two octets of the Docker subnets. Default: 10.89
  --upgrade             Move an install made by this script to the release, keeping .env.
  --restore FILE        Install from an export made on another host's Export installation page.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--domain) DOMAIN=${2:?--domain needs a value}; configured=1; shift ;;
	--local) LOCAL=1; configured=1 ;;
	--data-root) DATA_ROOT=${2:?--data-root needs a value}; configured=1; shift ;;
	--dir) DIR=${2:?--dir needs a value}; shift ;;
	--version) VERSION=${2:?--version needs a value}; shift ;;
	--http-port) HTTP_PORT=${2:?--http-port needs a value}; configured=1; shift ;;
	--https-port) HTTPS_PORT=${2:?--https-port needs a value}; configured=1; shift ;;
	--subnet-prefix) SUBNET_PREFIX=${2:?--subnet-prefix needs a value}; configured=1; shift ;;
	--upgrade) UPGRADE=1 ;;
	--restore) RESTORE=${2:?--restore needs a file}; configured=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option $1" ;;
	esac
	shift
done

[ -z "$UPGRADE" ] || [ -z "$configured" ] ||
	die "--upgrade takes only --dir and --version; the other settings stay as they are in $DIR/.env"
case "$DATA_ROOT" in /*) ;; *) die "--data-root must be an absolute path" ;; esac
case "$DIR" in /*) ;; *) die "--dir must be an absolute path" ;; esac
if [ -n "$RESTORE" ]; then
	case "$RESTORE" in /*) ;; *) RESTORE=$PWD/$RESTORE ;; esac
	[ -f "$RESTORE" ] && [ -r "$RESTORE" ] || die "cannot read the export $RESTORE"
fi
# Both end up unquoted in .env, which is sourced as shell.
case "$DATA_ROOT$DOMAIN" in *[!A-Za-z0-9._/-]*) die "--data-root and --domain may use only letters, digits and . _ / -" ;; esac
case "$SUBNET_PREFIX" in
*[!0-9.]* | *.*.* | .* | *.) die "--subnet-prefix takes two octets, such as 172.30" ;;
*.*) ;;
*) die "--subnet-prefix takes two octets, such as 172.30" ;;
esac

# Preflight: the host.

[ "$(id -u)" = 0 ] || die "run as root: curl ... | sudo sh"
[ "$(uname -m)" = x86_64 ] || die "the Valheim dedicated server runs on x86-64 only"
command -v curl >/dev/null || die "curl is not installed"
command -v docker >/dev/null || die "Docker is not installed: https://docs.docker.com/engine/install/"
docker info >/dev/null 2>&1 || die "cannot reach the Docker daemon; is it running?"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is not installed: https://docs.docker.com/compose/install/linux/"

# .install-complete holds the running release, written once the stack is healthy. A .env
# written by this script without it marks an unfinished install, which a rerun resumes.
if [ -n "$UPGRADE" ]; then
	[ -s "$DIR/.install-complete" ] ||
		die "no install made by this script in $DIR. Pass --dir, or upgrade by hand: $DOCS/operations.md#update-the-panel-or-game"
	current=$(cat "$DIR/.install-complete")
	DATA_ROOT=$(sed -n 's/^VALMIN_HOST_DATA_ROOT=//p' "$DIR/.env")
	[ -n "$DATA_ROOT" ] || die "$DIR/.env does not set VALMIN_HOST_DATA_ROOT"
else
	[ ! -e "$DIR/.install-complete" ] ||
		die "Valmin $(cat "$DIR/.install-complete") is already installed in $DIR. To upgrade it: curl ... | sudo sh -s -- --upgrade"
	resuming=
	grep -qs '^# Written by install.sh' "$DIR/.env" && resuming=1
	if [ -z "$resuming" ]; then
		if [ -n "$(docker ps -aq --filter label=com.docker.compose.project=valmin)" ]; then
			die "another Valmin stack already runs on this host. To upgrade it, see $DOCS/operations.md"
		fi
		[ ! -e "$DATA_ROOT/panel.db" ] || die "$DATA_ROOT already holds a Valmin database. To upgrade, see $DOCS/operations.md"
	fi
	if [ -e "$DATA_ROOT" ] && [ "$(stat -c '%u:%g' "$DATA_ROOT")" != 10000:10000 ]; then
		die "$DATA_ROOT exists and is not owned by 10000:10000. Remove it if it is empty, or choose another --data-root"
	fi

	# Preflight: the network.

	# A resumed install meets the networks its first run created, and those route its subnets.
	games_project=$(docker network inspect -f '{{index .Labels "com.docker.compose.project"}}' valmin-games 2>/dev/null) &&
		{ [ -z "$resuming" ] || [ "$games_project" != valmin ]; } &&
		die "a network named valmin-games already exists. Remove it first: docker network rm valmin-games"

	if [ -z "$resuming" ] && command -v ip >/dev/null; then
		for n in 13 14 15; do
			# Routes shorter than /8 are a full-tunnel VPN's default, not a claim on this range.
			if ip -4 route show match "$SUBNET_PREFIX.$n.1" |
				awk '$1 != "default" { n = split($1, a, "/"); if (n == 1 || a[2] >= 8) f = 1 } END { exit !f }'; then
				die "$SUBNET_PREFIX.$n.0/24 is already routed on this host (LAN, VPN or another Docker network). Choose another range: --subnet-prefix 172.30"
			fi
		done
	fi

	if grep -qs '"iptables": *false' /etc/docker/daemon.json; then
		echo "install: warning: Docker runs with iptables disabled, so published ports and container downloads will not work" >&2
	fi

	# Preflight: the address.

	if [ -z "$DOMAIN" ]; then
		route=$(ip -4 route get 1.1.1.1 2>/dev/null) || die "cannot detect this host's address; pass --domain"
		dev=$(echo "$route" | sed -n 's/.* dev \([^ ]*\).*/\1/p')
		DOMAIN=$(echo "$route" | sed -n 's/.* src \([^ ]*\).*/\1/p')
		case "$dev" in
		tun* | wg* | tailscale* | zt*) die "the default route goes through VPN interface $dev; pass --domain with the address browsers will use" ;;
		esac
		[ -n "$DOMAIN" ] || die "cannot detect this host's address; pass --domain"
		echo "Using $DOMAIN, the address of $dev. If browsers reach this host by another address, such as a cloud VM's public IP, pass --domain"
	fi
	case "$DOMAIN" in
	*:* | */*) die "--domain takes a hostname or an IPv4 address, without https://, a port or a path" ;;
	esac
	case "$DOMAIN" in
	*[!0-9.]*)
		if [ -z "$LOCAL" ] && ! getent ahosts "$DOMAIN" >/dev/null; then
			die "$DOMAIN does not resolve. Create its DNS record first, or pass --local for a LAN-only hostname"
		fi
		;;
	*) LOCAL=1 ;;
	esac

	if [ -z "$resuming" ] && command -v ss >/dev/null; then
		for spec in "t $HTTP_PORT" "t $HTTPS_PORT" "u $HTTPS_PORT"; do
			# shellcheck disable=SC2086
			set -- $spec
			[ -z "$(ss -H"$1"ln "sport = :$2")" ] && continue
			[ -n "$LOCAL" ] || die "port $2 is already in use on this host. A public certificate needs ports 80 and 443: free them, or use --local"
			die "port $2 is already in use on this host. Free it, or choose others: --http-port 8081 --https-port 8443"
		done
	fi
	if [ -z "$LOCAL" ] && [ "$HTTP_PORT:$HTTPS_PORT" != 80:443 ]; then
		echo "install: warning: a public certificate needs the router to forward public ports 80 and 443 to $HTTP_PORT and $HTTPS_PORT" >&2
	fi
fi

# Release and images.

fetch() {
	curl -fsSL --retry 3 --connect-timeout 10 "$@"
}

if [ -z "$VERSION" ]; then
	latest=$(fetch -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "cannot reach github.com; check this host's internet access or proxy"
	case "$latest" in
	*/releases/tag/*) VERSION=${latest##*/} ;;
	*) die "no published release yet; pass --version" ;;
	esac
fi

if [ -n "$UPGRADE" ]; then
	if [ "$VERSION" = "$current" ]; then
		echo "Valmin $current is already the requested release."
		exit 0
	fi
	# An older panel refuses a migrated database. Prereleases compare by their base version.
	newest=$(printf '%s\n' "${current%%-*}" "${VERSION%%-*}" | sort -V | tail -n 1)
	[ "$newest" = "${VERSION%%-*}" ] ||
		die "$VERSION is older than the running $current. Going back needs the database copy: $DOCS/operations.md#update-the-panel-or-game"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive=valmind_${VERSION#v}_linux_amd64.tar.gz
base=https://github.com/$REPO/releases/download/$VERSION
echo "Downloading Valmin $VERSION"
fetch -o "$tmp/$archive" "$base/$archive" || die "cannot download $base/$archive"
fetch -o "$tmp/checksums.txt" "$base/checksums.txt" || die "cannot download $base/checksums.txt"
(cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - >/dev/null) ||
	die "$archive does not match checksums.txt"

pin() {
	docker pull -q "$REGISTRY/$1:$VERSION" >/dev/null ||
		die "cannot pull $REGISTRY/$1:$VERSION. See $DOCS/troubleshooting.md#registry-says-denied-or-an-image-is-missing"
	docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' "$REGISTRY/$1:$VERSION" |
		grep -m1 "^$REGISTRY/$1@" || die "$REGISTRY/$1:$VERSION has no registry digest"
}
panel_image=$(pin valmind)
game_image=$(pin valheim)

# Configuration.

if [ -n "$UPGRADE" ]; then
	for f in compose.yaml Caddyfile; do
		cp -p "$DIR/$f" "$DIR/$f.old"
	done
	tar --no-same-owner -xzf "$tmp/$archive" -C "$DIR" --strip-components=1 deploy
	sed -i -e "s|^VALMIN_IMAGE=.*|VALMIN_IMAGE=$panel_image|" \
		-e "s|^VALMIN_GAME_IMAGE=.*|VALMIN_GAME_IMAGE=$game_image|" \
		"$DIR/.env"
else
	mkdir -p "$DIR"
	tar --no-same-owner -xzf "$tmp/$archive" -C "$DIR" --strip-components=1 deploy

	tls=
	[ -z "$LOCAL" ] || tls="'tls internal'"
	sed -e "s|^VALMIN_DOMAIN=.*|VALMIN_DOMAIN=$DOMAIN|" \
		-e "s|^VALMIN_HOST_DATA_ROOT=.*|VALMIN_HOST_DATA_ROOT=$DATA_ROOT|" \
		-e "s|^VALMIN_IMAGE=.*|VALMIN_IMAGE=$panel_image|" \
		-e "s|^VALMIN_GAME_IMAGE=.*|VALMIN_GAME_IMAGE=$game_image|" \
		-e "s|^VALMIN_TLS=.*|VALMIN_TLS=$tls|" \
		"$DIR/.env.example" >"$DIR/.env"
	{
		echo
		echo "# Written by install.sh"
		echo "VALMIN_HTTP_PORT=$HTTP_PORT"
		echo "VALMIN_HTTPS_PORT=$HTTPS_PORT"
		echo "VALMIN_SUBNET_PREFIX=$SUBNET_PREFIX"
		case "$DOMAIN" in
		*[!0-9.]*) [ -z "$LOCAL" ] || echo "VALMIN_HSTS=max-age=0" ;;
		esac
	} >>"$DIR/.env"

	url=https://$DOMAIN
	if [ "$HTTPS_PORT" = 443 ]; then
		rm -f "$DIR/compose.override.yaml"
	else
		url=$url:$HTTPS_PORT
		cat >"$DIR/compose.override.yaml" <<EOF
services:
  valmind:
    environment:
      VALMIN_SERVER_EXTERNAL_URL: $url
EOF
	fi
fi

# Host and stack.

echo "Preparing the host and pulling SteamCMD"
sh "$DIR/prepare-host.sh" "$DATA_ROOT" >/dev/null

cd "$DIR"
# A resumed install skips a restore that finished, and redoes one that did not. The installer
# reads the export as root and streams it, so the restore writes as uid 10000 without access to
# the file itself.
if [ -n "$RESTORE" ] && { [ ! -e "$DIR/.restore-complete" ] || [ ! -e "$DATA_ROOT/panel.db" ] ||
	[ -e "$DATA_ROOT/.restore-incomplete" ]; }; then
	echo "Restoring $RESTORE"
	docker compose run --rm --no-deps -T valmind valmind admin restore <"$RESTORE" ||
		die "the restore failed. Fix the cause above, then run this installer again; it starts the restore over"
	touch "$DIR/.restore-complete"
fi
echo "Starting Valmin"
up() {
	docker compose up -d --quiet-pull --wait --wait-timeout 300
}
if [ -n "$UPGRADE" ]; then
	up ||
		die "the stack did not start. Find the cause with: cd $DIR && docker compose logs valmind caddy. To go back to $current, see $DOCS/operations.md#update-the-panel-or-game"
	echo "$VERSION" >"$DIR/.install-complete"
	cat <<EOF

Valmin is upgraded from $current to $VERSION. Game servers kept running.

  The previous compose.yaml and Caddyfile are kept beside them as *.old; carry over any
  changes you made to them.
  If the release migrated the database, the previous one is in $DATA_ROOT/backups/panel-pre-*.db
EOF
	exit 0
fi
up ||
	die "the stack did not start. Find the cause with: cd $DIR && docker compose logs valmind caddy. Then run this installer again"
echo "$VERSION" >"$DIR/.install-complete"

if [ -n "$RESTORE" ]; then
	cat <<EOF

Valmin $VERSION is running the restored installation. Open $url and sign in with an existing
account. Each server is rebuilding its game installation under its Jobs; start the servers once
those finish.

  Files:      $DIR (.env holds the settings)
  Data:       $DATA_ROOT
  Old host:   keep its panel stopped, or both will write to the same remote backup destination.
EOF
	exit 0
fi

# Every start prints a new token and invalidates the previous one, so only the last counts.
docker compose logs --no-log-prefix valmind 2>/dev/null | awk '
	/Valmin first-run setup/ { block = ""; on = 1 }
	on { block = block $0 "\n" }
	on && /=====/ { on = 0 }
	END { printf "%s", block }'

cat <<EOF

Valmin $VERSION is running. Open $url and enter the setup token above.

  Files:      $DIR (.env holds the settings)
  Data:       $DATA_ROOT
  New token:  cd $DIR && docker compose restart valmind
  Upgrade:    curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sudo sh -s -- --upgrade
EOF
if [ -n "$LOCAL" ]; then
	cat <<EOF
  Certificate: browsers will warn until each device trusts Caddy's local CA:
              cd $DIR && docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt .
EOF
else
	echo "  Certificate: if the browser reports a TLS error, check: cd $DIR && docker compose logs caddy"
fi
cat <<EOF

Ports $HTTP_PORT and $HTTPS_PORT are published by Docker, which ufw does not filter.
After setup, run Diagnostics -> Run deep checks to confirm Steam and Thunderstore are reachable.
EOF
}
