package api

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// timeoutBody is what http.TimeoutHandler writes when a handler overruns. The message is
// fixed at construction, so it carries no request id — the X-Request-Id header on the same
// response does. TimeoutHandler answers 503, which is why this is unavailable rather than
// 11 §2.5's timeout, a code reserved for an upstream that did not answer.
const timeoutBody = `{"error":{"code":"unavailable",` +
	`"message":"The panel cannot do that right now.","request_id":""}}`

// Router is the panel's HTTP surface: the probes outside the chain, the API subtree behind
// it, and the rule that /api never falls through to the SPA.
type Router struct {
	mux    *http.ServeMux
	api    *http.ServeMux
	chain  []middleware.Layer
	within time.Duration
}

// NewRouter assembles the surface from the operator's settings. health is registered
// outside the chain: a probe is not an API client, and 11 §10 exempts both probes from the
// bootstrap gate, from authentication and from rate limiting (G5).
//
// bootstrapPending is the daemon's one DB read of 10 §6's real state, taken at startup —
// the gate this router builds only ever caches that answer in memory from here on
// (11 §5.3).
func NewRouter(
	cfg *config.Config, db *store.DB, health *Health, keeper *crypto.Keeper, bootstrapPending bool,
	engine *jobs.Engine, containerRuntime runtime.Runtime,
) (*Router, error) {
	external, err := url.Parse(cfg.Server.ExternalURL)
	if err != nil {
		return nil, fmt.Errorf("server.external_url: %w", err)
	}
	trusted := make([]netip.Prefix, 0, len(cfg.Server.TrustedProxies))
	for _, cidr := range cfg.Server.TrustedProxies {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxies %q: %w", cidr, err)
		}
		trusted = append(trusted, p)
	}

	gate := middleware.NewBootstrapGate(bootstrapPending)
	sessions := auth.NewSessions(db, cfg.Auth.SessionIdleTTL.Std(), cfg.Auth.SessionAbsoluteTTL.Std())

	rt := &Router{
		mux:    http.NewServeMux(),
		api:    http.NewServeMux(),
		within: cfg.Server.RequestTimeout.Std(),
		chain: middleware.Chain(&middleware.Config{
			TrustedProxies: trusted,
			ExternalURL:    external,
			BodyLimit:      cfg.Server.BodyLimitBytes,
			Keeper:         keeper,
			// Generous by design: this is the bug and flood guard every request passes,
			// not the login limit (11 §7).
			PerIP:     middleware.NewLimiter(300, time.Minute, 100),
			PerUser:   middleware.NewLimiter(300, time.Minute, 100),
			Bootstrap: gate,
			Auth:      sessions,
		}),
	}

	health.Routes(rt.mux)
	az := authz.New(db)
	(&Permissions{Authz: az, DB: db}).Routes(rt)
	NewAuth(auth.NewBootstrap(db), sessions, gate, keeper).Routes(rt)
	(&Users{DB: db, Sessions: sessions, Authz: az}).Routes(rt)
	NewInvites(
		db,
		auth.NewInvites(db, cfg.Auth.InviteTTL.Std()),
		sessions,
		az,
		keeper,
		cfg.Server.ExternalURL,
	).Routes(rt)
	(&Jobs{Engine: engine, Authz: az}).Routes(rt)
	(&Instances{DB: db, Authz: az, Runtime: containerRuntime, Keeper: keeper, Engine: engine, Cfg: cfg}).Routes(rt)
	// Registering /api/ here is what makes G4 structural: http.ServeMux takes the most
	// specific pattern, so a later "/" serving the SPA cannot swallow an API path and
	// answer a mistyped endpoint with 200 and a body of HTML.
	rt.mux.Handle("/api/", middleware.Apply(http.HandlerFunc(rt.dispatch), rt.chain))
	return rt, nil
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

// Handle registers an API route behind the request timeout of 11 §8.1. The pattern is the
// full path, method included: "GET /api/v1/instances".
func (rt *Router) Handle(pattern string, h http.Handler) {
	rt.api.Handle(pattern, http.TimeoutHandler(h, rt.within, timeoutBody))
}

// Stream registers a long-lived route: the console socket, a backup download. It gets no
// write deadline, because a server-wide one severs the console after thirty seconds and
// presents as "the console randomly disconnects" (C12, 11 §8.1).
//
// X-Accel-Buffering keeps nginx from spooling the response to its own disk before sending
// it. Everything else ignores the header, and it costs one line.
func (rt *Router) Stream(pattern string, h http.Handler) {
	rt.api.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Accel-Buffering", "no")
		h.ServeHTTP(w, r)
	}))
}

// dispatch hands the request to a registered API route, or answers 404 in the envelope.
//
// http.ServeMux would answer an unmatched path with text/plain, and 11 §1.1 has no
// endpoint that fails as a bare string. A path that exists under another method arrives
// here too and also reads as not_found, which is the direction ADR-038 already points.
// It asks the mux whether anything matched and then lets the mux serve, rather than
// invoking the handler it hands back: only ServeHTTP binds the wildcards, so calling the
// returned handler directly leaves every r.PathValue empty.
func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	if _, pattern := rt.api.Handler(r); pattern == "" {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	rt.api.ServeHTTP(w, r)
}
