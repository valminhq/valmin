#!/bin/sh
# Invokes the game binary directly — no start_server.sh / start_server_bepinex.sh (08 §4.1,
# ADR-019). instance.BuildSpec passes launch flags as argv (ADR-063); "$@" forwards them
# unmodified, so nothing here re-parses or re-joins a user-controlled string.
#
# SteamAppId is set by instance.BuildSpec via container.Config.Env, not here (03 §1.1).
set -eu
umask 002

cd /opt/valheim/server

# `↯` Modded-ness is a filesystem fact, not an Env one (ADR-107). container.Config.Env is
# fixed at create and a container is created exactly once, at provision (A1), so a vanilla
# instance that later gains BepInEx has to start loading it without being recreated. This
# block is what makes that true: the presence of the Doorstop library *is* the switch.
#
# The four names and this order are copied from the pack's own start_server_bepinex.sh
# (denikson-BepInExPack_Valheim-5.4.2333, read 1 Sep 2026), not inferred from the on-disk
# layout. That distinction is the whole of 03 §5.2: inferring them produced 4.x names on a
# 3.x layout, and the server booted perfectly, logged no error, and loaded zero mods.
#
# LD_PRELOAD is a bare soname, resolved through LD_LIBRARY_PATH — not a path. ./linux64 is
# prepended after ./doorstop_libs, so it ends up first, exactly as the pack's script leaves
# it.
if [ -f doorstop_libs/libdoorstop_x64.so ]; then
	export DOORSTOP_ENABLED=1
	export DOORSTOP_TARGET_ASSEMBLY=./BepInEx/core/BepInEx.Preloader.dll
	export LD_LIBRARY_PATH="./doorstop_libs:${LD_LIBRARY_PATH:-}"
	export LD_PRELOAD="libdoorstop_x64.so:${LD_PRELOAD:-}"
fi

export LD_LIBRARY_PATH="./linux64:${LD_LIBRARY_PATH:-}"

exec ./valheim_server.x86_64 "$@"
