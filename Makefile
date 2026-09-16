.POSIX:
VERSION ?=

.PHONY: build panel-image test test-integration test-integration-as-panel game-network lint fmt dev dev-setup clean stub-image game-image steamcmd-stub-image race fuzz release-snapshot release-check inventory web-install

GO       ?= go
NPM      ?= npm
WEB      := web
BIN      := bin/valmind
STUB     := valmin/valheim-stub:dev
GAME     := valmin/valheim:dev
PANEL    := valmin/valmind:dev
STEAMCMD := valmin/steamcmd-stub:dev
# The network every game container joins (ADR-190). Created here and by Compose, never by
# the panel: it reaches Docker through a socket proxy that denies the networks API.
GAMENET  ?= valmin-games

# Explicit, because `./...` descends into web/node_modules — some npm packages ship
# .go files and the go tool does not skip that directory.
PKGS    := ./cmd/... ./internal/... ./docker/... ./deploy/...

build: web-build
	$(GO) build -o $(BIN) ./cmd/valmind

web-install:
	cd $(WEB) && $(NPM) ci --no-audit --no-fund

web-build: web-install
	cd $(WEB) && $(NPM) run build

test: web-install
	$(GO) test $(PKGS)
	cd $(WEB) && $(NPM) test

# Real Docker daemon, stub images. Never the real ~1 GB game download (06 §4).
test-integration: web-build stub-image game-image steamcmd-stub-image panel-image game-network
	$(GO) test -tags=integration -count=1 $(PKGS)

game-network:
	@docker network inspect $(GAMENET) >/dev/null 2>&1 || docker network create $(GAMENET)

# The same suite under the panel's own uid, which is the only way one particular assertion
# runs at all: TestCreateInstanceProvisionsEndToEnd asserts A4's failure on any host whose
# uid is not 10000 — every dev machine and every CI runner — so provisioning's success
# branch never executes there. This target is what executes it. Needs `make dev-setup` once.
test-integration-as-panel: web-build stub-image game-image steamcmd-stub-image panel-image game-network
	@test -d $(DEV_DATA) || { echo "run 'make dev-setup' first (08 §2)"; exit 1; }
#	Absolute, because that is the path the go tool resolves. A relative probe passes on an
#	unreachable checkout: the kernel resolves it from the inherited cwd and never walks the
#	ancestors that deny search.
	@sudo -u $(DEV_USER) test -r $(CURDIR)/go.mod || { \
		echo "$(DEV_USER) cannot reach $(CURDIR) — the go tool would report a missing main"; \
		echo "module rather than a permission error. Run 'make dev-setup' (08 §2)."; exit 1; }
	sudo -u $(DEV_USER) -g $(DEV_USER) env \
		HOME=$(DEV_DATA) GOCACHE=$(DEV_DATA)/gocache \
		$(GO) test -tags=integration -count=1 $(PKGS)

stub-image:
	docker build -t $(STUB) docker/valheim-stub

# The real image carries no game files (08 §4), so building it costs nothing and needs no
# Steam egress — only the provisioning bind mount populates server/.
game-image:
	docker build -t $(GAME) docker/valheim

# The production panel image (08 §2, 10 §2): the static daemon with the SPA embedded, built
# entirely inside the image so the artefact does not depend on what is in the checkout.
# VERSION is the release identity; without it the build reports the commit the go tool stamped.
panel-image:
	docker build -t $(PANEL) --build-arg VERSION=$(VERSION) \
	    --build-arg COMMIT=$$(git rev-parse HEAD) -f docker/valmind/Dockerfile .

# Stands in for game.steamcmd_image in provisioning's integration tests (08 §3.2), never
# the real SteamCMD, which would need Steam egress and a ~1 GB download.
steamcmd-stub-image:
	docker build -t $(STEAMCMD) docker/steamcmd-stub

# The race detector over the boundaries where a data race costs world data rather than a
# flake: the archive writer and its swap, the job engine's locks and leases, and the config
# document that a mod install and an operator edit can reach at the same time.
race:
	$(GO) test -race -count=1 ./internal/backup/... ./internal/jobs/... ./internal/mods/...

# FUZZ_TIME per target, not in total. The corpus lives in the package (ADR-120); this target
# is the bounded run, not a replacement for the table tests beside it.
FUZZ_TIME ?= 30s

# .cfg parsing is the only boundary here that is a pure function over bytes, which is what
# fuzzing needs. The archive and recovery boundaries are covered by `make race` and by their
# own integration tests: both of those take a filesystem, not an input string.
fuzz:
	$(GO) test -run '^$$' -fuzz FuzzParseRoundTrip -fuzztime $(FUZZ_TIME) ./internal/mods/config
	$(GO) test -run '^$$' -fuzz FuzzSetKeepsEveryOtherByte -fuzztime $(FUZZ_TIME) ./internal/mods/config

# What this build depends on, from the module graph and the lockfile it actually used. It
# travels inside the release archive, so the checksums cover it and an operator auditing a
# published artefact does not have to trust a separate file to describe it.
inventory:
	@mkdir -p inventory
	$(GO) list -m all > inventory/go-modules.txt
	cd $(WEB) && $(NPM) ls --all > ../inventory/npm-packages.txt

# The release artefacts, built exactly as a tag builds them but published nowhere. This is
# what makes the release path a thing CI exercises rather than a thing a tag discovers.
release-snapshot:
	goreleaser release --snapshot --clean

# Asserts what the artefact actually contains, because the two ways it can be wrong are both
# silent: a binary with no SPA serves an unbuilt-SPA page, and a binary with no link-time
# identity reports "(devel)" to an operator quoting it in a bug report.
release-check: release-snapshot
	@bin=$$(find dist -type f -name valmind | head -1); \
	test -n "$$bin" || { echo "release-check: goreleaser produced no valmind binary"; exit 1; }; \
	out=$$("$$bin" version); \
	echo "$$out"; \
	case "$$out" in *"(devel)"*) \
		echo "release-check: the artefact carries no link-time version (internal/version)"; \
		exit 1;; esac; \
	grep -q _app/immutable "$$bin" || { \
		echo "release-check: the SPA is not embedded in the artefact (web/embed.go)"; exit 1; }; \
	ok=; for a in dist/*.tar.gz; do \
		list=$$(tar -tzf "$$a"); \
		case "$$list" in *deploy/compose.yaml*) ;; *) continue;; esac; \
		case "$$list" in *inventory/go-modules.txt*) ;; *) continue;; esac; \
		case "$$list" in *inventory/npm-packages.txt*) ;; *) continue;; esac; \
		ok=$$a; done; \
	test -n "$$ok" || { \
		echo "release-check: no archive carries both deploy/ (02 §5) and the inventory"; \
		exit 1; }; \
	test -f dist/checksums.txt || { echo "release-check: no checksums"; exit 1; }
	@echo "release-check: version, embedded SPA, deploy/, inventory and checksums all present"

lint: web-install
	golangci-lint run
	golangci-lint fmt --diff
	cd $(WEB) && $(NPM) run lint && $(NPM) run check

# golangci-lint owns formatting (gofumpt + gci + golines), not bare gofmt, or `fmt`
# and `lint` disagree about the same file.
fmt:
	golangci-lint fmt
	cd $(WEB) && $(NPM) run format

# `make dev` runs the SPA's dev server on :5173, proxying /api and the socket to the daemon
# on :8080 (see web/vite.config.ts). Every value below is overridable:
# `make dev DEV_DATA=...`.
#
# --strict-port is not tidiness. If :5173 is taken, Vite silently moves to :5174, the
# browser's Origin then stops matching server.external_url, and every request comes back
# 403 origin_rejected (D3) with nothing saying why. Fail on the port instead.
#
# Vite's proxy adds an `Access-Control-Allow-Origin` header to what it forwards. That is the
# dev server, not the panel: the daemon emits no `Access-Control-*` header under any
# configuration (D3), and a test asserts it. Seeing one in devtools here is not a bug.
#
# The stub images, not the real ones: a curious click on "Create server" would otherwise
# start a real ~1 GB SteamCMD download (06 §4). Provisioning still fails in dev, on purpose
# — the clone must run as uid 10000 (A4) and this process does not — so the wizard, the 202,
# live job progress and the error state are all exercised without downloading anything.
DEV_DATA ?= /srv/valmin-dev

# One port, two consumers. The SPA dev server binds it and the daemon is told the same
# origin — the panel sends no CORS headers under any configuration (D3) and the WebSocket
# upgrade requires a matching Origin (11 §6.3) — so a drift between these two is a 403 on
# every state-changing request. Overriding DEV_PORT moves both.
DEV_PORT ?= 5173
DEV_URL  ?= http://localhost:$(DEV_PORT)

# DEV_HOST is what the SPA dev server binds. It defaults to localhost, because a dev server
# on the LAN is a dev server anyone on the LAN can reach, and it exists because the panel is
# routinely run on a different machine from the browser: the daemon has to run as uid 10000
# on the host whose Docker it drives (08 §2), which is not necessarily the machine you are
# typing on. Serving the panel from a second box needs both halves moved together:
#
#   make dev DEV_HOST=0.0.0.0 DEV_URL=http://<that-box>:5173
#
# It is 5173 you browse, never the daemon's own 8080. Under `make dev` the daemon serves the
# SPA embedded at the last `make build`, which is stale by construction — vite serves the
# live one and proxies /api through. Hitting 8080 directly gets an old UI and an origin
# mismatch, which together look like a login bug.
DEV_HOST ?= localhost

# The daemon runs as uid 10000, the same uid every container runs as (08 §2). Container uids
# are host uids on a bind mount, so a panel writing as anyone else produces a server/ the
# game cannot write and a build cache SteamCMD cannot write. Running as your login account
# gets as far as "create server" and no further.
#
# Running the daemon in a container is production's actual shape and would also cover the
# panel image, the Docker socket mount and 10 §1.2's host_data_root round trip, none of
# which this target exercises. The uid setup below is the same either way.
DEV_UID  ?= 10000
DEV_USER ?= valmin

# The daemon binary is built into $(DEV_BIN), not bin/: $(DEV_USER) has to read and execute
# it, and a home directory is usually 0700 to every other account. That directory is owned
# by you and readable by everyone, so `make dev` needs no group membership and no sudo — the
# group below is for browsing worlds by hand (08 §2.1), not for running the panel.
DEV_BIN  ?= $(DEV_DATA)/bin/valmind

# One-time host setup for `make dev`. Needs root once; after it, `make dev` does not.
#
# DEV_ME is the developer, resolved so it is correct whether this runs as `make dev-setup`
# (the recipe sudo's each line itself) or as `sudo make dev-setup`. Under sudo, `id -u` is 0,
# which would create the bin directory owned by root — and `make dev`'s writability guard
# would then tell the operator to run `dev-setup`, which is what they had just done. sudo
# exports SUDO_UID and SUDO_USER for exactly this, and the fallback covers the un-sudo'd
# invocation.
DEV_ME   = $${SUDO_UID:-$$(id -u)}
DEV_MENAME = $${SUDO_USER:-$$(id -un)}

dev-setup:
	@getent group $(DEV_UID) >/dev/null || sudo groupadd -g $(DEV_UID) $(DEV_USER)
	@id -u $(DEV_USER) >/dev/null 2>&1 || \
		sudo useradd -u $(DEV_UID) -g $(DEV_UID) -M -s /usr/sbin/nologin $(DEV_USER)
	@sudo usermod -aG docker $(DEV_USER)
	@sudo install -d -o $(DEV_UID) -g $(DEV_UID) -m 2775 $(DEV_DATA)
	@sudo install -d -o $(DEV_ME) -g $(DEV_UID) -m 0755 $(dir $(DEV_BIN))
	@sudo usermod -aG $(DEV_USER) $(DEV_MENAME)
#	test-integration-as-panel runs the go tool as $(DEV_USER), so that account has to be able
#	to reach this checkout. A private home directory (0700, or 0710 as systemd-homed writes
#	it) denies it, and the failure names neither permissions nor the directory: the go tool
#	reports "does not contain main module or its selected dependencies" and the suite exits
#	before one test runs. Search only, and only on components that lack it — it grants no
#	listing of the home directory and no read of anything beside the checkout.
#
#	Best-effort: a filesystem without ACL support fails here and must not take `make dev` with
#	it, since that target reaches nothing under this path. The as-panel target refuses to run
#	on its own probe instead.
	@dir="$(CURDIR)"; path=; \
	while [ "$$dir" != "/" ]; do path="$$dir $$path"; dir=$$(dirname "$$dir"); done; \
	for dir in $$path; do \
		sudo -u $(DEV_USER) test -x "$$dir" 2>/dev/null && continue; \
		sudo setfacl -m u:$(DEV_USER):x "$$dir" || \
			echo "warning: could not grant $(DEV_USER) search on $$dir;" \
			     "make dev is unaffected, make test-integration-as-panel will refuse to run"; \
	done
	@echo "Done. $(DEV_DATA) is owned by $(DEV_USER) ($(DEV_UID)); $(dir $(DEV_BIN)) is yours."
	@echo "make dev works now; it needs no group membership."
	@echo "08 §2.1: the group is what lets you read and copy worlds by hand without sudo;"
	@echo "log out and back in (or 'newgrp $(DEV_USER)') for that part to take effect."

dev: game-network
	@test "$$(id -u)" != 0 || { \
		echo "run 'make dev' as yourself, not under sudo."; \
		echo "Only the daemon runs as $(DEV_USER) — the recipe elevates that one process."; \
		echo "Under sudo, npm writes web/node_modules/.vite as root and the next run fails"; \
		echo "with EACCES on a file you no longer own."; exit 1; }
#	The SteamCMD image has to be present before the daemon needs it. Docker does not pull on
#	container create, only on `docker run`, so a missing image surfaces as "No such image"
#	from inside a provision job — after three retries and thirty seconds, with the panel
#	reporting a failed download and nothing saying which image or why.
#
#	The stub is built, anything else is pulled, never the other way round. The
#	steamcmd-stub-image target tags whatever STEAMCMD names, so building it while STEAMCMD is
#	overridden to a real image would overwrite that tag with the stub, and the next provision
#	would "succeed" against a fake download.
	@docker image inspect $(STEAMCMD) >/dev/null 2>&1 || { \
		if [ "$(STEAMCMD)" = "valmin/steamcmd-stub:dev" ]; then \
			echo "building the SteamCMD stub ($(STEAMCMD))"; \
			$(MAKE) --no-print-directory steamcmd-stub-image; \
		else \
			echo "pulling $(STEAMCMD)"; \
			docker pull $(STEAMCMD) || { \
				echo "could not obtain $(STEAMCMD); provisioning would fail with 'No such image'"; \
				exit 1; }; \
		fi; }
#	The message names what is actually wrong and who owns it, rather than saying "run
#	dev-setup" — a dead end for anyone whose dev-setup had already run and produced a
#	root-owned directory (see DEV_ME above).
	@test -w $(dir $(DEV_BIN)) || { \
		echo "$(dir $(DEV_BIN)) is not writable by you (uid $$(id -u))."; \
		echo "It is owned by: $$(stat -c '%U:%G %a' $(dir $(DEV_BIN)) 2>/dev/null || echo 'it does not exist')"; \
		echo; \
		echo "Run 'make dev-setup' — WITHOUT sudo. The recipe elevates the lines that need it."; \
		echo "Running the whole thing under sudo is what makes this directory root's."; \
		exit 1; }
#	Authenticate before Vite starts writing to the terminal, so the sudo prompt stays visible.
	@sudo -v
#	vite is run directly rather than through `npm run dev`, and the subshell `exec`s it. Both
#	reasons are about Ctrl+C. npm answers SIGINT by exiting and orphaning its child, so the
#	vite npm started keeps port $(DEV_PORT) and the next `make dev` dies on "Port 5173 is
#	already in use". And the trap needs a real pid: `%1` is job control, which a
#	non-interactive shell does not have, so `kill %1` silently finds no such job. `exec` makes
#	$$! the pid of vite itself rather than of the subshell around it. If web/package.json's
#	`dev` script grows past `vite dev`, this line has to follow it.
	@( cd $(WEB) && exec ./node_modules/.bin/vite dev --strict-port --host $(DEV_HOST) --port $(DEV_PORT) ) & \
	web=$$!; \
	trap 'kill $$web 2>/dev/null' EXIT INT TERM; \
	$(GO) build -o $(DEV_BIN) ./cmd/valmind && \
	sudo -u $(DEV_USER) -g $(DEV_USER) env \
	VALMIN_DATA_ROOT=$(DEV_DATA) \
	VALMIN_DATA_HOST_ROOT=$(DEV_DATA) \
	VALMIN_SERVER_EXTERNAL_URL=$(DEV_URL) \
	VALMIN_GAME_IMAGE=$(GAME) \
	VALMIN_GAME_STEAMCMD_IMAGE=$(STEAMCMD) \
	VALMIN_GAME_NETWORK=$(GAMENET) \
	VALMIN_LOG_FORMAT=text \
	$(DEV_BIN)

clean:
	rm -rf bin dist inventory $(WEB)/build/app $(WEB)/.svelte-kit
