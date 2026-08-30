package middleware

import (
	"net/http"
	"sync/atomic"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// BootstrapGate is 11 §5.3: while a fresh panel has no admin, every route 503s except the
// ones this layer exempts. The flag is read from memory on every request and only ever
// written on change — not a database round trip per request.
type BootstrapGate struct {
	pending atomic.Bool
}

// NewBootstrapGate starts the gate at pending, the value auth.Bootstrap.Pending resolves
// at startup — computed once there because it needs the database; this type only ever
// caches the answer.
func NewBootstrapGate(pending bool) *BootstrapGate {
	g := &BootstrapGate{}
	g.pending.Store(pending)
	return g
}

// Complete flips the gate open, once, when the first admin is created. There is no path
// back (10 §6): nothing in this type re-closes it.
func (g *BootstrapGate) Complete() { g.pending.Store(false) }

// Pending reports the current cached state.
func (g *BootstrapGate) Pending() bool { return g.pending.Load() }

// exempt names the paths reachable while bootstrap is pending. 11 §5.3 also exempts
// /healthz, /readyz and the SPA's static assets, but none of those pass through this
// chain — the probes are registered outside it entirely (Health.Routes) and the SPA
// fallback lives on the outer mux, so /api/v1/setup is the only row this layer owns.
var exempt = map[string]bool{
	"/api/v1/setup": true,
}

// SetupGate is the chain row: everything but the exempt set answers 503 setup_required
// while pending (11 §5.1 row 7, 10 §6).
func SetupGate(g *BootstrapGate) Layer {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if g.Pending() && !exempt[r.URL.Path] {
				apierr.Write(w, r, apierr.New(apierr.SetupRequired))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
