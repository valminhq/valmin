.POSIX:
.PHONY: build test test-integration lint fmt dev clean stub-image game-image steamcmd-stub-image

GO       ?= go
NPM      ?= npm
WEB      := web
BIN      := bin/valmind
STUB     := valmin/valheim-stub:dev
GAME     := valmin/valheim:dev
STEAMCMD := valmin/steamcmd-stub:dev

# Explicit, because `./...` descends into web/node_modules — some npm packages ship
# .go files and the go tool does not skip that directory.
PKGS    := ./cmd/... ./internal/... ./docker/...

build: web-build
	$(GO) build -o $(BIN) ./cmd/valmind

web-build:
	cd $(WEB) && $(NPM) ci --no-audit --no-fund && $(NPM) run build

test:
	$(GO) test $(PKGS)
	cd $(WEB) && $(NPM) test

# Real Docker daemon + the STUB images. Never the real ~1 GB game download (06 §4).
test-integration: stub-image game-image steamcmd-stub-image
	$(GO) test -tags=integration -count=1 $(PKGS)

stub-image:
	docker build -t $(STUB) docker/valheim-stub

# The real image carries no game files (08 §4), so building it costs nothing and needs no
# Steam egress — only the provisioning bind mount populates server/ (WP-13).
game-image:
	docker build -t $(GAME) docker/valheim

# Stands in for game.steamcmd_image in provisioning's integration tests (08 §3.2) — never
# the real SteamCMD, which would need Steam egress and a ~1 GB download.
steamcmd-stub-image:
	docker build -t $(STEAMCMD) docker/steamcmd-stub

lint:
	golangci-lint run
	golangci-lint fmt --diff
	cd $(WEB) && $(NPM) run lint && $(NPM) run check

# golangci-lint owns formatting (gofumpt + gci + golines), not bare gofmt, or `fmt`
# and `lint` disagree about the same file.
fmt:
	golangci-lint fmt
	cd $(WEB) && $(NPM) run format

# `make dev` — the SPA's dev server on :5173 proxying /api and the socket to the daemon on
# :8080 (see web/vite.config.ts). Every value below is overridable: `make dev DEV_DATA=...`.
#
# `↯` --strict-port is not tidiness. If :5173 is taken Vite silently moves to :5174, the
# browser's Origin then stops matching server.external_url, and every request comes back
# 403 origin_rejected (D3, ADR-036) with nothing saying why. Fail on the port instead.
#
# `↯` Vite's proxy adds an `Access-Control-Allow-Origin` header to what it forwards. That is
# the dev server, not the panel: the daemon on :8080 emits no `Access-Control-*` header under
# any configuration (D3, ADR-036) and a test asserts it. Seeing it in devtools during
# `make dev` is not a bug to fix.
#
# `↯` The stub images, not the real ones: a curious click on "Create server" would otherwise
# start a real ~1 GB SteamCMD download (06 §4). Provisioning still fails in dev, on purpose —
# the clone must run as uid 10000 (A4, Q14) and this process is not — so the wizard, the 202,
# live job progress and the error state are all exercised without downloading anything.
DEV_DATA ?= $(HOME)/.valmin-dev
DEV_URL  ?= http://localhost:5173

dev:
	@mkdir -p $(DEV_DATA)
	@cd $(WEB) && $(NPM) run dev -- --strict-port & \
	trap 'kill %1 2>/dev/null' EXIT INT TERM; \
	VALMIN_DATA_ROOT=$(DEV_DATA) \
	VALMIN_DATA_HOST_ROOT=$(DEV_DATA) \
	VALMIN_SERVER_EXTERNAL_URL=$(DEV_URL) \
	VALMIN_GAME_IMAGE=$(GAME) \
	VALMIN_GAME_STEAMCMD_IMAGE=$(STEAMCMD) \
	VALMIN_LOG_FORMAT=text \
	$(GO) run ./cmd/valmind

clean:
	rm -rf bin $(WEB)/build/app $(WEB)/.svelte-kit
