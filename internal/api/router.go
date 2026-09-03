package api

import (
	"context"
	"crypto/subtle"
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
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/ws"
	"github.com/valminhq/valmin/web"
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
	// supervisor is 12 §9.1's recovery and the observer loop. It is built here because it
	// shares the instance handlers' dependencies exactly, and handed back rather than
	// started, so the daemon keeps 12 §9.1's ordering in one readable place.
	supervisor *Supervisor
	// mods is M2's Thunderstore sync scheduler (12 §11: a clock, not a worker). Handed
	// back the same way supervisor is, so the daemon starts its ticker after serving
	// begins rather than this package reaching into main's lifecycle.
	mods *Mods
	// spa serves the embedded single-page app on "/". It is a field behind a delegating
	// handler rather than registered directly, because http.ServeMux cannot re-register a
	// pattern and a test needs to stand a built SPA in front of the real routing.
	spa http.Handler
	// hub is handed back for the same reason: 11 §10 closes the sockets before
	// http.Server.Shutdown, which would otherwise wait out the whole grace period for
	// handlers that never return on their own.
	hub *ws.Hub
}

// Supervisor is the observer and crash-recovery driver (12 §1, §9.1). The daemon runs
// Recover before it serves and Run for the life of the process.
func (rt *Router) Supervisor() *Supervisor { return rt.supervisor }

// Mods is M2's Thunderstore sync scheduler. The daemon runs Run for the life of the
// process, the same way it runs the Supervisor's.
func (rt *Router) Mods() *Mods { return rt.mods }

// Hub is the WebSocket hub, for the shutdown sequence of 11 §10.
func (rt *Router) Hub() *ws.Hub { return rt.hub }

// SetSPA replaces the embedded single-page app, for tests that need one that exists.
func (rt *Router) SetSPA(h http.Handler) { rt.spa = h }

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
	streams := instance.NewStreams(containerRuntime)
	instances := &Instances{
		DB: db, Authz: az, Runtime: containerRuntime, Keeper: keeper, Engine: engine, Cfg: cfg,
		Streams: streams,
	}
	instances.Routes(rt)
	rt.supervisor = NewSupervisor(instances)

	rt.mods = &Mods{
		DB: db, Authz: az, Engine: engine,
		Client:       thunderstore.New(cfg.Thunderstore.BaseURL),
		Cache:        cache.New(cache.Root(cfg.Data.Root)),
		DataRoot:     cfg.Data.Root,
		SyncInterval: cfg.Thunderstore.SyncInterval.Std(),
	}
	rt.mods.Routes(rt)
	// The create wizard installs mods through the mod engine, which is built after the
	// instance handlers that use it (Q42).
	instances.Mods = rt.mods

	socks := &sockets{engine: engine, streams: streams}
	rt.hub = ws.New(&ws.Config{
		Origin: external.Scheme + "://" + external.Host,
		CSRF: func(sessionID, token string) bool {
			want, err := middleware.CSRFToken(keeper, sessionID)
			return err == nil && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
		},
		Authz: az,
		Res:   resolver{db: db},
		Src:   ws.Sources{Console: socks.console, Stats: socks.stats, Job: socks.job},
		SessionExpiry: func(ctx context.Context, sessionID string) (time.Time, error) {
			return db.SessionAbsoluteExpiry(ctx, sessionID)
		},
	})
	// A Stream route, so no server-wide write deadline severs the console thirty seconds
	// in (C12, 11 §8.1).
	rt.Stream("GET /api/v1/ws", rt.hub)
	// 14 §6: a revoked session has to reach the socket it left open, not merely the next
	// request it will never make.
	sessions.OnRevoke(func(sessionID, userID string) {
		if sessionID != "" {
			rt.hub.SessionRevoked(sessionID)
		}
		if userID != "" {
			rt.hub.UserRevoked(userID)
		}
	})
	// 14 §4.4: the engine publishes a transition in the same moment it writes one, from the
	// two places its transactions commit.
	engine.Announce(announceState(db, rt.hub))
	rt.supervisor.hub = rt.hub
	// Registering /api/ here is what makes G4 structural: http.ServeMux takes the most
	// specific pattern, so a later "/" serving the SPA cannot swallow an API path and
	// answer a mistyped endpoint with 200 and a body of HTML.
	rt.mux.Handle("/api/", middleware.Apply(http.HandlerFunc(rt.dispatch), rt.chain))
	rt.spa = SPA(web.Assets)
	rt.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.spa.ServeHTTP(w, r)
	}))
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
