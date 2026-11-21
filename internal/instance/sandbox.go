package instance

import "github.com/valminhq/valmin/internal/config"

// Sandbox is the panel-wide part of a game container's isolation: the OCI runtime it runs
// under and the resolv.conf it mounts. The zero value is the Docker daemon's default runtime
// with Docker's own DNS.
type Sandbox struct {
	// Runtime is the Docker runtime name; empty is the daemon's default.
	Runtime string
	// ResolvConf is the host path of the resolv.conf mounted read-only into the container;
	// empty mounts none.
	ResolvConf string
}

// RuntimeName is how a message names an OCI runtime, empty being the daemon's default.
func RuntimeName(name string) string {
	if name == "" {
		return "Docker's default"
	}
	return name
}

// SandboxOf reads the sandbox every game container is created with from game.runtime and
// game.dns.
func SandboxOf(cfg *config.Config) Sandbox {
	return Sandbox{Runtime: cfg.Game.Runtime, ResolvConf: cfg.GameResolvConf()}
}
