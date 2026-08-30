# Runtime environment for a Valheim dedicated server (08 §4). Carries no game files — the
# install arrives via a per-instance bind mount at provision time (08 §3, WP-13).
#
# Base matches the M0 measurement host (README, debian:trixie-slim): the Steam-distributed
# binary and Unity's native libraries are glibc-linked, not musl-compatible.
FROM debian:trixie-slim

RUN groupadd -g 10000 valmin \
    && useradd -u 10000 -g valmin -M -s /usr/sbin/nologin valmin

COPY entrypoint.sh /usr/local/bin/valmin-entrypoint
RUN chmod 0755 /usr/local/bin/valmin-entrypoint

USER valmin
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/local/bin/valmin-entrypoint"]
