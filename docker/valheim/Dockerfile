# Runtime environment for a Valheim dedicated server (08 §4). It carries no game files: the
# install arrives through a per-instance bind mount at provision time (08 §3).
#
# debian-slim rather than alpine because the Steam-distributed binary and Unity's native
# libraries are glibc-linked and not musl-compatible.
FROM debian:trixie-slim

# ca-certificates is required, not garnish. debian-slim ships no CA bundle, and the game
# makes HTTPS calls of its own the moment `-crossplay` is on: ZNet.GetPublicIP fetches the
# server's public address from api.ipify.org and friends. With no trust store every one of
# those handshakes fails, the server never learns its own IP, never registers with PlayFab,
# and never reaches `Game server connected` — logging only `Could not extract valid IP
# address from externalIP download string`, hundreds of times, which reads as a network or
# IPv6 fault and is neither.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# -m, not -M: useradd records /home/valmin in /etc/passwd either way, and -M skips creating
# it. The game then resolves HOME to a directory that does not exist and cannot create it,
# because every container drops all capabilities (08 §5) so uid 10000 cannot write /home.
RUN groupadd -g 10000 valmin \
    && useradd -u 10000 -g valmin -m -s /usr/sbin/nologin valmin

COPY entrypoint.sh /usr/local/bin/valmin-entrypoint
RUN chmod 0755 /usr/local/bin/valmin-entrypoint

USER valmin
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/local/bin/valmin-entrypoint"]
