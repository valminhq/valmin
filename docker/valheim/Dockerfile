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
# `↯` ca-certificates, and it is not optional garnish. debian-slim ships **no** CA bundle,
# and the game makes HTTPS calls of its own the moment `-crossplay` is on: ZNet.GetPublicIP
# fetches the server's public address from api.ipify.org and friends. With no trust store
# every one of those TLS handshakes fails, the server never learns its own IP, never
# registers with PlayFab, and never reaches `Game server connected` — while logging nothing
# but `Could not extract valid IP address from externalIP download string`, hundreds of
# times, which reads as a network or IPv6 problem and is neither.
#
# Found 3 Sep 2026, after two days of blaming the host: the operator's own image happened to
# carry a CA bundle and ours never has, so every comparison between "the panel" and "by
# hand" was really a comparison between two images. The correlation is exact — CA bundle
# present, crossplay registers; absent, it does not — and it holds on both machines.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -g 10000 valmin \
    && useradd -u 10000 -g valmin -m -s /usr/sbin/nologin valmin

COPY entrypoint.sh /usr/local/bin/valmin-entrypoint
RUN chmod 0755 /usr/local/bin/valmin-entrypoint

USER valmin
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/local/bin/valmin-entrypoint"]
