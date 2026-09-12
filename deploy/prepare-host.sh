#!/bin/sh
# Prepares a host for the panel: the data root, its ownership, and the group that lets the
# operator read their own worlds without sudo (08 §2.1).
#
# Idempotent — running it twice changes nothing the second time. It creates what is missing
# and refuses what is wrong; it never takes ownership of data that is already there.
set -eu

UID_VALMIN=10000
GID_VALMIN=10000
ROOT="${1:-/srv/valmin}"

die() {
	echo "prepare-host: $*" >&2
	exit 1
}

[ "$(id -u)" = 0 ] || die "run as root: it creates a group and a directory owned by uid $UID_VALMIN"

# The group is what makes ADR-006's point survive panel-owned files: add your own login
# account to it and `cp` still works on a world directory.
if ! getent group "$GID_VALMIN" >/dev/null; then
	groupadd -g "$GID_VALMIN" valmin
	echo "created group valmin ($GID_VALMIN)"
fi
if ! getent passwd "$UID_VALMIN" >/dev/null; then
	# The panel runs as this uid inside its container; the host account exists so the
	# numbers have names in `ls -l` and so the group has a member.
	useradd -u "$UID_VALMIN" -g "$GID_VALMIN" -M -s /usr/sbin/nologin valmin
	echo "created user valmin ($UID_VALMIN)"
fi

if [ -e "$ROOT" ]; then
	[ -d "$ROOT" ] || die "$ROOT exists and is not a directory"
	owner=$(stat -c '%u:%g' "$ROOT")
	# `↯` Refused, never corrected. A recursive chown over an existing data root is how a
	# half-finished migration silently rewrites a world tree, and the ownership that is
	# already there is evidence about how this panel has been running (A3, A4).
	[ "$owner" = "$UID_VALMIN:$GID_VALMIN" ] || die \
		"$ROOT is owned by $owner, not $UID_VALMIN:$GID_VALMIN.
Move it aside or correct it deliberately; this script will not chown data it did not create."
	echo "$ROOT already prepared"
else
	# setgid, so everything created inside inherits the group. Umask 002 in the image is the
	# other half (08 §2.1).
	install -d -o "$UID_VALMIN" -g "$GID_VALMIN" -m 2775 "$ROOT"
	echo "created $ROOT"
fi

[ -S /var/run/docker.sock ] || die "no /var/run/docker.sock on this host"

# The game and SteamCMD images, acquired here because nothing else acquires them. Compose
# pulls its own three services and the panel never pulls (ADR-048): it creates containers
# from images it expects to be present, so on a fresh host the startup self-check fails for
# want of the game image and the first provision fails for want of SteamCMD — each as "No
# such image", from inside a job, naming neither this script nor the variable.
env_file="$(dirname "$0")/.env"
if [ -f "$env_file" ]; then
	# The same file Compose reads, so the images pulled here are the images that will run.
	# shellcheck source=/dev/null
	. "$env_file"
fi
: "${VALMIN_STEAMCMD_IMAGE:=steamcmd/steamcmd:latest}"
[ -n "${VALMIN_GAME_IMAGE:-}" ] || die \
	"VALMIN_GAME_IMAGE is not set. Fill in $env_file first: it names the game image this
host will run, and both this script and Compose read it."

for image in "$VALMIN_GAME_IMAGE" "$VALMIN_STEAMCMD_IMAGE"; do
	if docker image inspect "$image" >/dev/null 2>&1; then
		echo "$image already present"
	else
		echo "pulling $image"
		docker pull "$image" || die "could not pull $image"
	fi
done

echo
echo "VALMIN_HOST_DATA_ROOT=$ROOT"
echo
echo "Check that matches .env, then: docker compose up -d"
echo "Add yourself to the valmin group to read worlds by hand: usermod -aG $GID_VALMIN <you>"
