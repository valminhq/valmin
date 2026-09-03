# Runtime environment for a Valheim dedicated server (08 §4). Carries no game files — the
# install arrives via a per-instance bind mount at provision time (08 §3, WP-13).
#
# Base matches the M0 measurement host (README, debian:trixie-slim): the Steam-distributed
# binary and Unity's native libraries are glibc-linked, not musl-compatible.
FROM debian:trixie-slim

# `↯` -m, not -M. useradd records /home/valmin in /etc/passwd either way, and -M skips
# creating it — so the game resolved HOME to a directory that did not exist, could not
# create it (every container drops all capabilities, 08 §5, so uid 10000 cannot write /home),
# and logged `CreateDirectory '/home/valmin' failed` on every boot. Reported 3 Sep 2026.
#
# `↯` No test caught it because the **stub image was more correct than this one**: alpine's
# `adduser -D` creates the home directory, so the image whose stated job is to mirror this
# one's runtime contract (08 §2, 08 §4.2) quietly satisfied an invariant this one broke.
# image_test.go now asserts it here, where it failed.
RUN groupadd -g 10000 valmin \
    && useradd -u 10000 -g valmin -m -s /usr/sbin/nologin valmin

COPY entrypoint.sh /usr/local/bin/valmin-entrypoint
RUN chmod 0755 /usr/local/bin/valmin-entrypoint

USER valmin
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/local/bin/valmin-entrypoint"]
