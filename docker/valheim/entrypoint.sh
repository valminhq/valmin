#!/bin/sh
# Invokes the game binary directly — no start_server.sh / start_server_bepinex.sh (08 §4.1,
# ADR-019). instance.BuildSpec passes launch flags as argv (ADR-063); "$@" forwards them
# unmodified, so nothing here re-parses or re-joins a user-controlled string.
#
# SteamAppId is set by instance.BuildSpec via container.Config.Env, not here (03 §1.1).
set -eu
umask 002

export LD_LIBRARY_PATH="./linux64:${LD_LIBRARY_PATH:-}"
cd /opt/valheim/server

exec ./valheim_server.x86_64 "$@"
