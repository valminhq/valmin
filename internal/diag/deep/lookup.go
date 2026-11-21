package deep

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/runtime"
)

const (
	lookupPurpose = "name-lookup-check"
	// lookupName is a public name every game server resolves: Steam's Web API.
	lookupName = "api.steampowered.com"
	// lookupSeconds bounds the lookup, which a resolver that never answers stretches to its
	// own timeouts and retries.
	lookupSeconds = 15
	// exitNotFound is getent's code for a name it could not resolve.
	exitNotFound = 2
)

// probeLookup resolves lookupName from a throwaway container in the game image on the game
// network, set up as a game container is, and grades the outcome.
func probeLookup(ctx context.Context, rt runtime.Runtime, cfg *config.Config) diag.Observation {
	var stderr strings.Builder
	code, err := runtime.RunThrowaway(ctx, rt, &runtime.ThrowawaySpec{
		Purpose: lookupPurpose,
		Root:    cfg.Data.HostRoot,
		Image:   cfg.Game.Image,
		Entrypoint: []string{
			"/usr/bin/timeout", strconv.Itoa(lookupSeconds), "/usr/bin/getent", "hosts", lookupName,
		},
		User:       strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Network:    cfg.Game.Network,
		OCIRuntime: cfg.Game.Runtime,
		Binds:      cfg.GameResolvBinds(),
		Stderr:     &stderr,
	})
	obs := diag.Observation{CheckedAt: time.Now().UTC(), Source: diag.SourceJob, Target: diag.LookupTarget(cfg)}
	switch {
	case err != nil:
		obs.Status, obs.Detail, obs.Diagnostic = diag.StatusUnknown, "The probe could not run.", err.Error()
	case code == 0:
		obs.Status, obs.OK = diag.StatusOK, true
		obs.Detail = "A game server resolved " + lookupName + "."
	case code == exitNotFound, code == exitTimedOut:
		obs.Status = diag.StatusWarn
		obs.Detail = "A game server could not resolve " + lookupName + ". Crossplay and mods that " +
			"look up names fail while this does."
	default:
		obs.Status = diag.StatusUnknown
		obs.Detail = fmt.Sprintf("The lookup of %s exited %d without a recognised answer.", lookupName, code)
		obs.Diagnostic = strings.TrimSpace(stderr.String())
	}
	return obs
}
