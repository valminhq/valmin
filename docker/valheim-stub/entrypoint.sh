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

stamp() { date -u '+%m/%d/%Y %H:%M:%S'; }

# Prefixed grammar: the networking subsystem's own timestamp.
netlog() { echo "$(stamp): $1"; }

# Bare grammar: Unity Debug.Log.
log() { echo "$1"; }

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
	log "[Message:   BepInEx] BepInEx 5.4.23.3 - valheim_server (stub)"
	log "[Message:   BepInEx] Preloader started"
	log "[Info   :   BepInEx] Patching [UnityEngine.CoreModule] with [BepInEx.Chainloader]"
	log "[Message:   BepInEx] Chainloader ready"
	log "[Message:   BepInEx] Chainloader started"
	# "plugin" is singular at 1; the pattern must tolerate both (03 §5.3, E9).
	if [ "$STUB_PLUGINS" = "1" ]; then
		log "[Info   :   BepInEx] 1 plugin to load"
		log "[Info   :   BepInEx] Loading [Jotunn 2.29.2]"
	else
		log "[Info   :   BepInEx] ${STUB_PLUGINS} plugins to load"
	fi
	log "[Message:   BepInEx] Chainloader startup complete"
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
