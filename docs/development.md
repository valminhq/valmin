# Development

[Documentation](README.md) / Development

Use the Go version in [go.mod](../go.mod) (currently `1.27.0`), Node.js 24 or 26 as
tested in CI — [web/.nvmrc](../web/.nvmrc) names the one the release artefact is built
on — npm, Make, and Docker. For `make lint`, install the golangci-lint
version specified in [CI](../.github/workflows/ci.yml).

For a persistent panel used by other people, follow [installation](installation.md).
`make dev` is an interactive development setup with a separate data directory.

From the repository root:

```sh
make build
make dev-setup
make stub-image
make dev GAME=valmin/valheim-stub:dev
```

`make build` installs frontend dependencies, builds the SPA, and embeds it in
`bin/valmind`. `make dev-setup` uses sudo to create the development account and
`/srv/valmin-dev`, grant Docker access, and prepare filesystem permissions.
This is separate from production's `prepare-host.sh` and `/srv/valmin`.
Run these Make targets as your own user; do not run the whole target under sudo.

Open `http://localhost:5173`. Vite proxies API and WebSocket traffic to the daemon
on port 8080. The stub setup exercises panel workflows without downloading or
running Valheim.

To develop against the real game:

```sh
make game-image
make dev STEAMCMD=steamcmd/steamcmd:latest
```

This uses the same development data directory. To keep stub data apart from real
servers, set `DEV_DATA` on both `make dev-setup` and `make dev` to a separate path.
For a daemon on another machine, use an SSH tunnel so the browser still reaches
`http://localhost:5173`:

```sh
ssh -N -L 5173:localhost:5173 YOUR_USER@YOUR_HOST
```

Non-localhost HTTP origins cannot store the panel's Secure session cookies. For
direct remote browser access, serve the dev frontend through HTTPS and set
`DEV_URL` to that HTTPS origin.

| Command                          | Checks                                                                       |
| -------------------------------- | ---------------------------------------------------------------------------- |
| `make test`                      | Go and frontend unit tests. `test-go` and `test-web` run one half.           |
| `make lint`                      | Go lint and formatting, frontend lint, and Svelte type checks. `lint-go` and `lint-web` run one half. |
| `make test-integration`          | Tests using real Docker and stub game downloads; builds the required images. `INTEGRATION_JOBS` sets how many packages and tests run at once (default 8). |
| `make test-integration-as-panel` | Integration tests as UID 10000; requires `make dev-setup`.                   |
| `make images`                    | The integration images alone. `save-images` and `load-images` move them between machines as `images.tar`. |
| `make race`                      | Race checks for backups, jobs, and mods.                                     |
| `make fuzz FUZZ_TIME=30s`        | Bounded configuration parser fuzzing.                                        |

Backend code lives in `cmd/valmind` and `internal`; frontend code lives in `web`.
Deployment files are in `deploy`, and container definitions are in `docker`.
See [Architecture](architecture.md) for how the components fit together and
[the API guide](api.md) for the browser/backend contract.
