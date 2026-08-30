# Stand-in for SteamCMD in integration tests (08 §3.2). Accepts the same argv shape as the
# real steamcmd binary and writes a minimal fake install instead of downloading ~1 GB from
# Steam — mirrors docker/valheim-stub's own reason for existing (06 §4).
FROM alpine:3.22

COPY entrypoint.sh /usr/local/bin/steamcmd
RUN chmod 0755 /usr/local/bin/steamcmd

ENTRYPOINT ["/usr/local/bin/steamcmd"]
