#!/bin/sh
# Stand-in for `steamcmd +force_install_dir /out +login anonymous +app_update 896660
# [-beta <branch> [-betapassword <pw>]] validate +quit`. Writes a minimal fake install into /out
# instead of reaching Steam, so instance.EnsureBuildCached's whole pipeline can run against a
# real daemon without the real ~1 GB download.
#
# public and default_old install the builds app-info.vdf lists. Any other branch installs
# 21981591 when given a password and fails as Steam does without one. A run with no
# +app_update, the panel's priming login, does nothing.
#
# STEAMCMD_STUB_EXIT_CODE simulates a failed run (a bad login, a full disk, a kill) without
# needing a real failure condition.
set -eu

STEAMCMD_STUB_EXIT_CODE="${STEAMCMD_STUB_EXIT_CODE:-0}"

echo "Steam Console Client (c) Valve Corporation - stub"
echo "-- type 'quit' to exit --"
echo "Loading Steam API...OK."

if [ "$STEAMCMD_STUB_EXIT_CODE" != "0" ]; then
	echo "ERROR! Failed to install app '896660' (stub failure)"
	exit "$STEAMCMD_STUB_EXIT_CODE"
fi

update=no branch=public password=
while [ $# -gt 0 ]; do
	case "$1" in
	+app_info_print)
		cat /usr/local/share/app-info.vdf
		exit 0
		;;
	+app_update) update=yes ;;
	-beta)
		branch="$2"
		shift
		;;
	-betapassword)
		password="$2"
		shift
		;;
	esac
	shift
done
[ "$update" = yes ] || exit 0

case "$branch" in
public) build=21981590 ;;
default_old) build=20460518 ;;
*)
	if [ -z "$password" ]; then
		echo "ERROR! Failed to set beta '$branch'"
		exit 8
	fi
	build=21981591
	;;
esac

mkdir -p /out/linux64 /out/steamapps
printf '#!/bin/sh\necho stub valheim server\n' >/out/valheim_server.x86_64
chmod 0755 /out/valheim_server.x86_64
echo "896660" >/out/steam_appid.txt
{
	printf '"AppState"\n{\n\t"appid" "896660"\n\t"buildid" "%s"\n' "$build"
	[ "$branch" = public ] || printf '\t"UserConfig"\n\t{\n\t\t"BetaKey" "%s"\n\t}\n' "$branch"
	printf '}\n'
} >/out/steamapps/appmanifest_896660.acf

echo "Success! App '896660' fully installed."
exit 0
