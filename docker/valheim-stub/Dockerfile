# Stub Valheim server for integration tests. Mirrors the real image's runtime contract
# (08 §2, 08 §4.2) without the ~1 GB game download.
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

RUN addgroup -g 10000 valmin && adduser -D -u 10000 -G valmin valmin

COPY entrypoint.sh /usr/local/bin/valmin-entrypoint
RUN chmod 0755 /usr/local/bin/valmin-entrypoint

USER valmin
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/local/bin/valmin-entrypoint"]
