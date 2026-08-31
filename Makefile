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

dev:
	cd $(WEB) && $(NPM) run dev &
	$(GO) run ./cmd/valmind

clean:
	rm -rf bin $(WEB)/build/app $(WEB)/.svelte-kit
