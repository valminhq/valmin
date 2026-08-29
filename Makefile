.POSIX:
.PHONY: build test test-integration lint fmt dev clean stub-image

GO      ?= go
NPM     ?= npm
WEB     := web
BIN     := bin/valmind
STUB    := valmin/valheim-stub:dev

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

# Real Docker daemon + the STUB image. Never the real ~1 GB game download (06 §4).
test-integration: stub-image
	$(GO) test -tags=integration -count=1 $(PKGS)

stub-image:
	docker build -t $(STUB) docker/valheim-stub

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
	rm -rf bin $(WEB)/build $(WEB)/.svelte-kit
