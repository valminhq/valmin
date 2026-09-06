#!/bin/sh
# Stand-in for `steamcmd +force_install_dir /out +login anonymous +app_update 896660
# validate +quit` (08 §3.2). Writes a minimal fake install into /out instead of reaching
# Steam, so instance.EnsureBuildCached's whole pipeline can run against a real daemon
# without the real ~1 GB download.
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

for arg in "$@"; do
	if [ "$arg" = "+app_info_print" ]; then
		cat /usr/local/share/app-info.vdf
		exit 0
	fi
done

mkdir -p /out/linux64
printf '#!/bin/sh\necho stub valheim server\n' >/out/valheim_server.x86_64
chmod 0755 /out/valheim_server.x86_64
echo "896660" >/out/steam_appid.txt
mkdir -p /out/steamapps
cat /usr/local/share/appmanifest.acf >/out/steamapps/appmanifest_896660.acf

echo "Success! App '896660' fully installed."
exit 0
