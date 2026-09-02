#!/bin/sh
# Stand-in for the Valheim dedicated server in integration tests. Emits the log lines
# measured in M0 (03 §3.2.1, 03 §3.5, 03 §5.3) and runs the same SIGINT shutdown path.
#
# Networking lines carry an MM/DD/YYYY HH:MM:SS: prefix and Unity Debug.Log lines do
# not; both grammars appear below on purpose (03 §3.5).
#
# STUB_MODE selects the scenario:
#   normal          boot, announce readiness, idle until signalled
#   no-ready        boot but never announce readiness (12 §3.3 fallback path)
#   exit-early      exit non-zero shortly after boot (start failure)
#   no-save-finish  on SIGINT stop after "finishing", never reaching "finished" (B2)
#   modded          normal, plus the BepInEx chainloader sequence (03 §5.3)
#
# Modded-ness is also detected the way the real entrypoint detects it (ADR-107): the
# presence of doorstop_libs/libdoorstop_x64.so under the server bind. STUB_MODE=modded still
# forces the sequence for this image's own tests, which run it without a server mount.
set -eu

STUB_MODE="${STUB_MODE:-normal}"
STUB_PLUGINS="${STUB_PLUGINS:-1}"
STUB_SAVE_DELAY="${STUB_SAVE_DELAY:-0}"
STUB_ZDOS="${STUB_ZDOS:-21771}"

# Mirrors the real entrypoint's Doorstop autodetection, so an integration test proves the
# contract rather than a flag the test itself set. Kept separate from STUB_MODE so the
# other scenarios stay orthogonal to it — a modded instance that never boots ready is
# exactly what the E1 assertion has to be tested against.
STUB_MODDED=0
[ "$STUB_MODE" = "modded" ] && STUB_MODDED=1
[ -f /opt/valheim/server/doorstop_libs/libdoorstop_x64.so ] && STUB_MODDED=1

SERVER_DIR=/opt/valheim/server
PLUGIN_DIR="$SERVER_DIR/BepInEx/plugins"

# BepInEx writes its own log file and only *also* writes to stdout when the console key is
# enabled (03 §5.5) — and the panel's load verification reads the file, not the stream
# (ADR-110). A stub that only printed would leave that path with no coverage at all.
#
# Truncated per boot. Whether the real BepInEx truncates or appends is unmeasured; the
# panel's parser takes the last chainloader run either way, so the stub picks the simpler
# one and neither answer is asserted anywhere.
BEPINEX_LOG=""
if [ "$STUB_MODDED" = "1" ] && mkdir -p "$SERVER_DIR/BepInEx" 2>/dev/null &&
	: >"$SERVER_DIR/BepInEx/LogOutput.log" 2>/dev/null; then
	BEPINEX_LOG="$SERVER_DIR/BepInEx/LogOutput.log"
fi

stamp() { date -u '+%m/%d/%Y %H:%M:%S'; }

# Prefixed grammar: the networking subsystem's own timestamp.
netlog() { echo "$(stamp): $1"; }

# Bare grammar: Unity Debug.Log.
log() { echo "$1"; }

# A loader line: to the stream, and to the loader's own log file when there is one.
blog() {
	log "$1"
	[ -n "$BEPINEX_LOG" ] && printf '%s\n' "$1" >>"$BEPINEX_LOG"
	return 0
}

# The plugins this server would load: the `.dll` files actually sitting under
# BepInEx/plugins/, discovered rather than configured. `↯` That is what makes the load
# verification test mean something — nothing in the test tells the stub what to announce,
# so the names come from what the installer really placed.
discovered_plugins() {
	[ -d "$PLUGIN_DIR" ] || return 0
	find "$PLUGIN_DIR" -name '*.dll' 2>/dev/null | sort | while read -r dll; do
		base="${dll##*/}"
		echo "${base%.dll}"
	done
}

shutdown() {
	log "Game - OnApplicationQuit"
	log "Available space to current user: 161039331328. Saving is blocked if below: 6665246 bytes. Warnings are given if below: 13330492"
	log "Shutting down"
	log "ZNet Shutdown"
	log "PrepareSave: clone done in 4ms"
	log "PrepareSave: ZDOExtraData.PrepareSave done in 17 ms"
	[ "$STUB_SAVE_DELAY" -gt 0 ] && sleep "$STUB_SAVE_DELAY"
	log "World save writing starting"
	log "World save writing started"
	log "Saved ${STUB_ZDOS} ZDOs"
	log "World save writing finishing"
	if [ "$STUB_MODE" = "no-save-finish" ]; then
		exit 0
	fi
	log "World save writing finished"
	exit 0
}
trap shutdown INT TERM

log "Starting Valheim stub (mode=${STUB_MODE})"

if [ "$STUB_MODDED" = "1" ]; then
	blog "[Message:   BepInEx] BepInEx 5.4.23.3 - valheim_server (stub)"
	blog "[Message:   BepInEx] Preloader started"
	blog "[Info   :   BepInEx] Patching [UnityEngine.CoreModule] with [BepInEx.Chainloader]"
	blog "[Message:   BepInEx] Chainloader ready"
	blog "[Message:   BepInEx] Chainloader started"

	FOUND="$(discovered_plugins)"
	if [ -n "$FOUND" ]; then
		# What is on disk wins over STUB_PLUGINS: a server announces the plugins it has.
		COUNT="$(printf '%s\n' "$FOUND" | wc -l | tr -d ' ')"
		WORD=plugins
		[ "$COUNT" = "1" ] && WORD=plugin
		blog "[Info   :   BepInEx] ${COUNT} ${WORD} to load"
		printf '%s\n' "$FOUND" | while read -r name; do
			blog "[Info   :   BepInEx] Loading [${name} 1.0.0]"
		done
	# "plugin" is singular at 1; the pattern must tolerate both (03 §5.3, E9).
	elif [ "$STUB_PLUGINS" = "1" ]; then
		blog "[Info   :   BepInEx] 1 plugin to load"
		blog "[Info   :   BepInEx] Loading [Jotunn 2.29.2]"
	else
		blog "[Info   :   BepInEx] ${STUB_PLUGINS} plugins to load"
	fi
	blog "[Message:   BepInEx] Chainloader startup complete"
fi

if [ "$STUB_MODE" = "exit-early" ]; then
	sleep 2 &
	wait $!
	log "Stub exiting non-zero to simulate a failed start"
	exit 1
fi

netlog "IPv4, returning 127.0.0.1:${STUB_PORT:-2456}"

if [ "$STUB_MODE" != "no-ready" ]; then
	netlog "Game server connected"
fi

# Idle in the background so the trap runs promptly instead of after sleep returns.
while true; do
	sleep 3600 &
	wait $!
done
