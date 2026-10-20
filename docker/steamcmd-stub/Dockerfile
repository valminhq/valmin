# Stand-in for SteamCMD in integration tests (08 §3.2). Accepts the same argv shape as the
# real steamcmd binary and writes a minimal fake install instead of downloading ~1 GB from
# Steam — mirrors docker/valheim-stub's own reason for existing (06 §4).
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# Matches valheim-stub and the uid every container runs as (08 §2): a stub that installs
# as root would hide the ownership failure A4 exists to catch.
RUN addgroup -g 10000 valmin && adduser -D -u 10000 -G valmin valmin

COPY app-info.vdf /usr/local/share/app-info.vdf
COPY entrypoint.sh /usr/local/bin/steamcmd
RUN chmod 0755 /usr/local/bin/steamcmd

USER valmin
ENTRYPOINT ["/usr/local/bin/steamcmd"]
