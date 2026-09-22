# The panel image (08 §2, 10 §2). It carries the daemon, the SPA embedded inside it, and
# nothing else the code does not call.
#
# The SPA is built first so the Go stage can embed it: web/embed.go embeds all:build, and a
# binary compiled without it serves a page telling the operator to run `make build`.
FROM node:24-trixie-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/build ./web/build

# The release identity, empty for a local build, which then reports the commit the go tool
# stamped (internal/version).
ARG VERSION=""
# The revision, passed in because this stage has no repository to read: .dockerignore keeps
# .git out of the build context, so the go tool cannot stamp it (internal/version).
ARG COMMIT=""

# CGO_ENABLED=0 for a static binary: the runtime stage is a different base, and a daemon
# that stops running when a shared library moves under it is a deployment failure with no
# error message. -trimpath keeps the builder's paths out of the artefact.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
        -ldflags "-s -w \
            -X github.com/valminhq/valmin/internal/version.Version=${VERSION} \
            -X github.com/valminhq/valmin/internal/version.Commit=${COMMIT}" \
        -o /out/valmind ./cmd/valmind

FROM debian:trixie-slim

# ca-certificates, because everything the panel reaches is HTTPS: the Thunderstore API
# (03 §6.1), Steam's public build endpoint (12 §2.5) and webhook destinations, which are
# HTTPS by policy (ADR-167). debian-slim ships no trust store.
#
# `↯` coreutils is why this is debian-slim and not distroless or busybox: provisioning
# clones the game with `cp -a --reflink=auto` (08 §3), and only GNU cp has that flag. A
# base without it turns every provision into a failure at the clone step.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates coreutils \
    && rm -rf /var/lib/apt/lists/*

# uid 10000 for the panel and every game container alike (A3, 08 §2): container uids are host
# uids on a bind mount, and the panel owns every file it writes into one.
RUN groupadd -g 10000 valmin \
    && useradd -u 10000 -g valmin -m -s /usr/sbin/nologin valmin

COPY --from=build /out/valmind /usr/local/bin/valmind
COPY docker/valmind/entrypoint.sh /usr/local/bin/valmin-entrypoint
RUN chmod 0755 /usr/local/bin/valmind /usr/local/bin/valmin-entrypoint

USER valmin

# The probe is the daemon itself (11 §10): one request to /healthz, which touches neither
# the database nor Docker. The alternative is curl in the image for one request.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/valmind", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/valmin-entrypoint"]
CMD ["/usr/local/bin/valmind"]
