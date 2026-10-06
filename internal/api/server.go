package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/instance/history"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/notify/delivery"
	"github.com/valminhq/valmin/internal/ratelimit"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/ws"
	"github.com/valminhq/valmin/web"
)

// Dependencies supplies the process-owned components used by the HTTP surface.
type Dependencies struct {
	Config           *config.Config
	DB               *store.DB
	Keeper           *crypto.Keeper
	BootstrapPending bool
	Engine           *jobs.Engine
	Runtime          runtime.Runtime
	Options          *Options
}

// Options substitutes transport components during server construction.
type Options struct {
	SPA           http.Handler
	WebhookSender *notify.Sender
}

// Server owns the HTTP surface and its process-lifetime services.
type Server struct {
	router        *Router
	routes        []routeSpec
	health        *Health
	supervisor    *control.Supervisor
	instances     *Instances
	mods          *Mods
	scheduler     *scheduler.Scheduler
	players       *history.Recorder
	webhooks      *Webhooks
	notifier      *delivery.Notifier
	remoteBackups *RemoteBackups
	diagnostics   *Diagnostics
	hub           *ws.Hub
}

// Handler returns the constructed HTTP surface.
func (s *Server) Handler() http.Handler { return s.router }

// Recover reconciles interrupted instance work before requests are accepted.
func (s *Server) Recover(ctx context.Context) error {
	if err := s.supervisor.Recover(ctx); err != nil {
		return fmt.Errorf("recover instances: %w", err)
	}
	return nil
}

// Run blocks until all process-lifetime loops exit after ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){
		s.supervisor.Run, s.mods.Run, s.scheduler.Run, s.notifier.Dispatcher.Run,
		s.remoteBackups.worker().Run, s.players.Run,
	} {
		wg.Go(func() { run(ctx) })
	}
	wg.Wait()
}

// Drain makes readiness fail before the listener shuts down.
func (s *Server) Drain() { s.health.Drain() }

// CloseSockets closes active WebSockets before HTTP shutdown.
func (s *Server) CloseSockets() { s.hub.Close() }

// NewServer assembles the HTTP surface and process-lifetime services without starting work.
func NewServer(d Dependencies) (*Server, error) { return newServer(&d, nil) }

func newServer(d *Dependencies, extraRoutes []routeSpec) (*Server, error) {
	cfg, db, keeper := d.Config, d.DB, d.Keeper
	engine, containerRuntime := d.Engine, d.Runtime
	health := &Health{DB: db, Runtime: containerRuntime}
	srv := &Server{health: health}
	external, trusted, err := parseServerNetwork(cfg)
	if err != nil {
		return nil, err
	}

	var sender *notify.Sender
	if d.Options != nil {
		sender = d.Options.WebhookSender
	}
	if sender == nil {
		sender = &notify.Sender{}
	}
	gate := middleware.NewBootstrapGate(d.BootstrapPending)
	sessions := auth.NewSessions(db, cfg.Auth.SessionIdleTTL.Std(), cfg.Auth.SessionAbsoluteTTL.Std())

	rt := &Router{
		mux:    http.NewServeMux(),
		api:    http.NewServeMux(),
		within: cfg.Server.RequestTimeout.Std(),
	}

	srv.router = rt
	healthRoutes(rt.mux, health)
	routes := &routeTable{}
	az := authz.New(db)
	commands := command.NewManager(db, containerRuntime, keeper)
	permissionRoutes(routes, &Permissions{Authz: az, DB: db, Commands: commands})
	authRoutes(routes, NewAuth(auth.NewBootstrap(db), sessions, gate, keeper))
	userRoutes(routes, &Users{DB: db, Sessions: sessions, Authz: az})
	grants := &Grants{DB: db, Authz: az}
	grantRoutes(routes, grants)
	auditRoutes(routes, &Audit{DB: db, Authz: az})
	inviteRoutes(routes, NewInvites(
		db,
		auth.NewInvites(db, cfg.Auth.InviteTTL.Std()),
		sessions,
		az,
		keeper,
		cfg.Server.ExternalURL,
	))
	jobRoutes(routes, &Jobs{Engine: engine, Authz: az, DB: db})
	keyRoutes(routes, &Keys{DB: db, Authz: az, Engine: engine, Keeper: keeper})
	srv.webhooks = &Webhooks{
		DB: db, Authz: az, Engine: engine, Keeper: keeper, Sender: sender,
	}
	srv.notifier = &delivery.Notifier{
		DB: db, Dispatcher: srv.webhooks.dispatcher(),
	}
	webhookRoutes(routes, srv.webhooks)
	srv.remoteBackups = &RemoteBackups{DB: db, Authz: az, Engine: engine, Keeper: keeper, Cfg: cfg}
	remoteBackupRoutes(routes, srv.remoteBackups)
	alertRuleRoutes(routes, &AlertRules{DB: db, Authz: az})
	streams := instance.NewStreams(containerRuntime)
	srv.players = history.New(db)
	streams.OnPlayers = srv.players.Observe
	streams.OnIdentity = srv.players.Identified
	// Outside the API chain and outside the API subtree, on a thin chain of its own: it is the
	// only route a stranger can reach, and it authorizes on a column rather than a session
	// (ADR-156). Its limiter is its own, so a flood of status reads cannot spend the budget
	// the login route shares.
	publicStatusRoutes(rt.mux, middleware.PublicChain(
		trusted, ratelimit.New(publicStatusPerMinute, time.Minute, publicStatusBurst)),
		&PublicStatus{DB: db, Streams: streams})
	instances := &Instances{
		DB: db, Authz: az, Runtime: containerRuntime, Keeper: keeper, Engine: engine, Cfg: cfg,
		Streams: streams, Commands: commands,
	}
	srv.instances = instances
	instanceRoutes(routes, instances)
	srv.supervisor = newControlSupervisor(instances, srv)

	schedules := srv.configureModsAndSchedules(cfg, db, az, engine, commands, instances, routes)
	instances.Operations = instances.newOperationService()
	srv.wireHub(external, keeper, az, db, engine, streams, grants, schedules, instances, routes, sessions)
	registerCancellationPolicies(engine)
	srv.finishRouter(d, extraRoutes, rt, routes, trusted, external, keeper, gate, sessions)
	return srv, nil
}

func registerCancellationPolicies(engine *jobs.Engine) {
	engine.RegisterCancelPolicy(jobs.KindProvision, control.ProvisionCancelPolicy)
	engine.RegisterCancelPolicy(jobs.KindGameUpdate, control.GameUpdateCancelPolicy)
	engine.RegisterCancelPolicy(jobs.KindUpdateCheck, control.UpdateCheckCancelPolicy)
	engine.RegisterCancelPolicy(jobs.KindClone, control.CloneCancelPolicy)
	engine.RegisterCancelPolicy(jobs.KindModInstall, manager.InstallCancelPolicy)
	engine.RegisterCancelPolicy(jobs.KindModUninstall, manager.UninstallCancelPolicy)
	engine.RegisterCancelPolicy(jobs.KindModToggle, manager.ToggleCancelPolicy)
}

func parseServerNetwork(cfg *config.Config) (*url.URL, []netip.Prefix, error) {
	external, err := url.Parse(cfg.Server.ExternalURL)
	if err != nil {
		return nil, nil, fmt.Errorf("server.external_url: %w", err)
	}
	trusted := make([]netip.Prefix, 0, len(cfg.Server.TrustedProxies))
	for _, cidr := range cfg.Server.TrustedProxies {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, nil, fmt.Errorf("server.trusted_proxies %q: %w", cidr, err)
		}
		trusted = append(trusted, p)
	}

	return external, trusted, nil
}

func (srv *Server) configureModsAndSchedules(
	cfg *config.Config, db *store.DB, az *authz.Authz, engine *jobs.Engine,
	commands *command.Manager, instances *Instances, routes *routeTable,
) *Schedules {
	// One client and one zip cache per enabled registry (03 §6.1). A disabled registry is
	// simply absent from both maps, so nothing downstream asks whether it is on.
	clients := map[source.Source]*thunderstore.Client{
		source.Thunderstore: thunderstore.New(cfg.Thunderstore.BaseURL),
	}
	if cfg.Hexium.Enabled {
		clients[source.Hexium] = thunderstore.NewBare(cfg.Hexium.BaseURL)
	}
	caches := make(map[source.Source]*cache.Cache, len(clients))
	for src := range clients {
		caches[src] = cache.New(cache.RootFor(cfg.Data.Root, src))
	}

	srv.mods = &Mods{
		DB: db, Authz: az, Engine: engine,
		Clients:      clients,
		SyncInterval: cfg.Thunderstore.SyncInterval.Std(),
	}
	srv.mods.plan = &manager.Planner{DB: db, Enabled: srv.mods.enabledSources()}
	srv.mods.install = &manager.Installer{
		DB: db, Engine: engine, Commands: commands,
		Clients: clients, Caches: caches, DataRoot: cfg.Data.Root,
	}
	modRoutes(routes, srv.mods)
	// The create wizard installs mods through the mod engine, which is built after the
	// instance handlers that use it (Q42).
	instances.Mods = srv.mods.install
	instances.Snapshotter = &control.Snapshotter{DataRoot: cfg.Data.Root, Runtime: instances.Runtime}
	srv.mods.install.ArchiveWorlds = instances.Snapshotter.Snapshot

	// Reuse the enabled registry clients for live diagnostics.
	srv.diagnostics = &Diagnostics{
		Instances: instances, Packages: clients[source.Thunderstore], StartedAt: time.Now().UTC(),
	}
	if client := clients[source.Hexium]; client != nil {
		srv.diagnostics.Hexium = client
	}
	diagnosticRoutes(routes, srv.diagnostics)

	schedules := &Schedules{DB: db, Authz: az, Instances: instances}
	scheduleRoutes(routes, schedules)
	srv.scheduler = &scheduler.Scheduler{
		DB: db, Interval: scheduleTickInterval, Enqueue: schedules.Enqueue,
		Occupied: schedules.Occupied, Held: schedules.announce, Warn: schedules.Warn,
	}

	return schedules
}

func (srv *Server) wireHub(
	external *url.URL, keeper *crypto.Keeper, az *authz.Authz, db *store.DB,
	engine *jobs.Engine, streams *instance.Streams, grants *Grants,
	schedules *Schedules, instances *Instances, routes *routeTable, sessions *auth.Sessions,
) {
	socks := &sockets{engine: engine, streams: streams}
	srv.hub = ws.New(&ws.Config{
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
	grants.Changes = srv.hub
	schedules.Hub = srv.hub
	// The join code is latched by the log reader, so the announcement starts there rather
	// than in a handler: a code that only reaches the browser on its next page load is one
	// an operator reads after the session it names has ended (Q25).
	streams.OnJoinCode = srv.hub.PublishJoinCode
	// A Stream route, so no server-wide write deadline severs the console thirty seconds
	// in (C12, 11 §8.1).
	routes.Stream("GET /api/v1/ws", srv.hub)
	// 14 §6: a revoked session has to reach the socket it left open, not merely the next
	// request it will never make.
	sessions.OnRevoke(func(sessionID, userID string) {
		if sessionID != "" {
			srv.hub.SessionRevoked(sessionID)
		}
		if userID != "" {
			srv.hub.UserRevoked(userID)
		}
	})
	// 14 §4.4: the engine publishes a transition in the same moment it writes one, from the
	// two places its transactions commit.
	engine.Announce(announceState(db, srv.hub))
	// Q52: a definition chain's progress is recorded in the finish transaction of the step
	// that completed it, so a crash cannot lose a step that landed.
	// One hook, two owners: the chain records its step and the notifier records what it owes,
	// both inside the finish transaction. A notification never changes a job's outcome, so the
	// second is ordered last and reports nothing back.
	engine.OnFinish(srv.onJobFinished)
	instances.Notify = srv.notifier
}

func (srv *Server) onJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	if err := srv.instances.operationService().OnJobFinished(ctx, tx, fin); err != nil {
		return err //nolint:wrapcheck // preserve the operation's original job error
	}
	return srv.notifier.OnJobFinished(ctx, tx, fin) //nolint:wrapcheck // preserve the notifier's original job error
}

func newControlSupervisor(inst *Instances, srv *Server) *control.Supervisor {
	return &control.Supervisor{
		DB: inst.DB, Engine: inst.Engine, Runtime: inst.Runtime, Keeper: inst.Keeper, Streams: inst.Streams,
		DataRoot: inst.Cfg.Data.Root, HostRoot: inst.Cfg.Data.HostRoot,
		StopTimeout: inst.Cfg.Game.StopTimeout.Std(), ReadyTimeout: inst.Cfg.Jobs.ReadyTimeout.Std(),
		PublishState: func(id, state string, restart bool) {
			if srv.hub != nil {
				srv.hub.PublishState(id, state, restart)
			}
		},
		NotifyUnexpectedStop: func(ctx context.Context, row *store.Instance, to, reason string) {
			if inst.Notify != nil {
				inst.Notify.NotifyUnexpectedStop(ctx, row, to, reason)
			}
		},
		SubmitStart: func(ctx context.Context, row *store.Instance, containerID string) error {
			_, err := inst.submitStart(ctx, row, containerID, "", nil)
			return err
		},
		SubmitUpdateCheck: func(ctx context.Context, scheduleID string) error {
			_, err := inst.updateChecker().Submit(ctx, scheduleID)
			if err != nil {
				return fmt.Errorf("submit update check: %w", err)
			}
			return nil
		},
		SubmitProvision: func(ctx context.Context, run *control.ProvisionRun) error {
			_, err := inst.submitProvision(ctx, run, instance.StateProvisioning)
			return err
		},
		SubmitDelete: func(ctx context.Context, row *store.Instance, keepWorlds bool) error {
			_, err := inst.submitDelete(ctx, row, keepWorlds, "")
			return err
		},
	}
}

func (srv *Server) finishRouter(
	d *Dependencies, extraRoutes []routeSpec, rt *Router, routes *routeTable, trusted []netip.Prefix,
	external *url.URL, keeper *crypto.Keeper, gate *middleware.BootstrapGate,
	sessions *auth.Sessions,
) {
	routes.routes = append(routes.routes, extraRoutes...)
	srv.routes = append([]routeSpec(nil), routes.routes...)
	large := routes.install(rt)
	rt.chain = middleware.Chain(&middleware.Config{
		TrustedProxies: trusted,
		ExternalURL:    external,
		BodyLimit:      d.Config.Server.BodyLimitBytes,
		LargeBody: func(r *http.Request) bool {
			_, pattern := rt.api.Handler(r)
			return large[pattern]
		},
		Keeper:    keeper,
		PerIP:     ratelimit.New(300, time.Minute, 100),
		PerUser:   ratelimit.New(300, time.Minute, 100),
		Bootstrap: gate,
		Auth:      sessions,
	})
	// Registering /api/ here is what makes G4 structural: http.ServeMux takes the most
	// specific pattern, so a later "/" serving the SPA cannot swallow an API path and
	// answer a mistyped endpoint with 200 and a body of HTML.
	rt.mux.Handle("/api/", middleware.Apply(http.HandlerFunc(rt.dispatch), rt.chain))
	if d.Options != nil {
		rt.spa = d.Options.SPA
	}
	if rt.spa == nil {
		rt.spa = SPA(web.Assets)
	}
	rt.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.spa.ServeHTTP(w, r)
	}))
}
