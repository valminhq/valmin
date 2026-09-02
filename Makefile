.POSIX:
.PHONY: build test test-integration test-integration-as-panel lint fmt dev dev-setup clean stub-image game-image steamcmd-stub-image

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

# `↯` The same suite as the panel's own uid, which is the only way one particular assertion
# runs at all: TestCreateInstanceProvisionsEndToEnd asserts A4's *failure* on any host whose
# uid is not 10000 — every dev machine and every CI runner — so provisioning's success branch
# has never executed anywhere. This target is what executes it. Needs `make dev-setup` once.
test-integration-as-panel: stub-image game-image steamcmd-stub-image
	@test -d $(DEV_DATA) || { echo "run 'make dev-setup' first (08 §2)"; exit 1; }
	sudo -u $(DEV_USER) -g $(DEV_USER) env \
		HOME=$(DEV_DATA) GOCACHE=$(DEV_DATA)/gocache \
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
DEV_DATA ?= /srv/valmin-dev

# `↯` One port, two consumers. The SPA dev server binds it and the daemon is told the same
# origin — the panel sends no CORS headers under any configuration (D3, ADR-036) and the
# WebSocket upgrade requires a matching Origin (11 §6.3), so a drift between these two is a
# 403 on every state-changing request. Overriding DEV_PORT moves both.
DEV_PORT ?= 5173
DEV_URL  ?= http://localhost:$(DEV_PORT)

# `↯` The daemon runs as uid 10000, the same uid every container runs as (08 §2). That is
# not a preference: container uids *are* host uids on a bind mount, so a panel writing as
# anyone else produces a server/ the game cannot write and a build cache SteamCMD cannot
# write. Running as your login account gets as far as "create server" and no further.
#
# `↯` Migrate to running the daemon in a container when a panel image exists (deployment
# work, currently unscheduled): that is production's actual shape, and it would also cover
# the panel image, the Docker socket mount and 10 §1.2's host_data_root round-trip, none of
# which this target exercises. The uid setup below is the same either way, so nothing here
# is wasted when that happens.
DEV_UID  ?= 10000
DEV_USER ?= valmin

# `↯` The daemon binary is built into $(DEV_BIN), not bin/: $(DEV_USER) has to read and
# execute it, and a home directory is usually 0700 to every other account. That directory is
# owned by **you** and readable by everyone, so `make dev` needs no group membership and no
# sudo — the group below is for browsing worlds by hand (08 §2.1), not for running the panel.
DEV_BIN  ?= $(DEV_DATA)/bin/valmind

# One-time host setup for `make dev`. Needs root once; after it, `make dev` does not.
dev-setup:
	@getent group $(DEV_UID) >/dev/null || sudo groupadd -g $(DEV_UID) $(DEV_USER)
	@id -u $(DEV_USER) >/dev/null 2>&1 || \
		sudo useradd -u $(DEV_UID) -g $(DEV_UID) -M -s /usr/sbin/nologin $(DEV_USER)
	@sudo usermod -aG docker $(DEV_USER)
	@sudo install -d -o $(DEV_UID) -g $(DEV_UID) -m 2775 $(DEV_DATA)
	@sudo install -d -o $$(id -u) -g $(DEV_UID) -m 0755 $(dir $(DEV_BIN))
	@sudo usermod -aG $(DEV_USER) $$(id -un)
	@echo "Done. $(DEV_DATA) is owned by $(DEV_USER) ($(DEV_UID)); $(dir $(DEV_BIN)) is yours."
	@echo "make dev works now; it needs no group membership."
	@echo "08 §2.1: the group is what lets you read and copy worlds by hand without sudo;"
	@echo "log out and back in (or 'newgrp $(DEV_USER)') for that part to take effect."

dev:
	@test -w $(dir $(DEV_BIN)) || { echo "run 'make dev-setup' first (08 §2)"; exit 1; }
	@cd $(WEB) && $(NPM) run dev -- --strict-port --port $(DEV_PORT) & \
	trap 'kill %1 2>/dev/null' EXIT INT TERM; \
	$(GO) build -o $(DEV_BIN) ./cmd/valmind && \
	sudo -u $(DEV_USER) -g $(DEV_USER) env \
	VALMIN_DATA_ROOT=$(DEV_DATA) \
	VALMIN_DATA_HOST_ROOT=$(DEV_DATA) \
	VALMIN_SERVER_EXTERNAL_URL=$(DEV_URL) \
	VALMIN_GAME_IMAGE=$(GAME) \
	VALMIN_GAME_STEAMCMD_IMAGE=$(STEAMCMD) \
	VALMIN_LOG_FORMAT=text \
	$(DEV_BIN)

clean:
	rm -rf bin $(WEB)/build/app $(WEB)/.svelte-kit
