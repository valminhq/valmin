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

	"github.com/valminhq/valmin/internal/alerts/scan"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup/remotecopy"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/discord"
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
	Options          Options
}

// Options substitutes transport components during server construction. The zero value uses
// the production ones.
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
	shutdowns     *scheduler.Shutdowns
	players       *history.Recorder
	webhooks      *Webhooks
	notifier      *delivery.Notifier
	remoteBackups *RemoteBackups
	diagnostics   *Diagnostics
	discord       *discord.Bot
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
		s.supervisor.Run, s.mods.Run, s.scheduler.Run, s.shutdowns.Run, s.notifier.Dispatcher.Run,
		s.remoteBackups.worker.Run, s.players.Run, s.discord.Run,
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
func NewServer(d *Dependencies) (*Server, error) { return newServer(d, nil) }

// wiring holds what every part of the server is built from.
type wiring struct {
	cfg      *config.Config
	db       *store.DB
	keeper   *crypto.Keeper
	engine   *jobs.Engine
	runtime  runtime.Runtime
	az       *authz.Authz
	commands *command.Manager
	sessions *auth.Sessions
	routes   *routeTable
}

// newServer builds every component once, in dependency order. extraRoutes registers test-only
// handlers behind the same chain as the API.
func newServer(d *Dependencies, extraRoutes []routeSpec) (*Server, error) {
	external, trusted, err := parseServerNetwork(d.Config)
	if err != nil {
		return nil, err
	}
	w := &wiring{
		cfg: d.Config, db: d.DB, keeper: d.Keeper, engine: d.Engine, runtime: d.Runtime,
		az:       authz.New(d.DB),
		commands: command.NewManager(d.DB, d.Runtime, d.Keeper),
		sessions: auth.NewSessions(d.DB, d.Config.Auth.SessionIdleTTL.Std(), d.Config.Auth.SessionAbsoluteTTL.Std()),
		routes:   &routeTable{},
	}
	health := &Health{DB: w.db, Runtime: w.runtime}
	rt := &Router{mux: http.NewServeMux(), api: http.NewServeMux(), within: w.cfg.Server.RequestTimeout.Std()}
	healthRoutes(rt.mux, health)
	gate := middleware.NewBootstrapGate(d.BootstrapPending)
	grants := w.accountRoutes(gate)

	webhooks, notifier := w.notifications(d.Options.WebhookSender)
	remoteBackups := w.remoteBackups()
	alertRuleRoutes(w.routes, &AlertRules{DB: w.db, Authz: w.az})

	streams := instance.NewStreams(w.runtime)
	notifier.JoinCode = func(id string) string {
		if r := streams.Reader(id); r != nil {
			return r.JoinCode()
		}
		return ""
	}
	players := history.New(w.db)
	streams.OnPlayers = players.Observe
	streams.OnIdentity = players.Identified
	// Outside the API chain and outside the API subtree, on a thin chain of its own: it is the
	// only route a stranger can reach, and it authorizes on a column rather than a session. Its
	// limiter is its own, so a flood of status reads cannot spend the login route's budget.
	publicStatusRoutes(rt.mux, middleware.PublicChain(
		trusted, ratelimit.New(publicStatusPerMinute, time.Minute, publicStatusBurst)),
		&PublicStatus{DB: w.db, Streams: streams})
	hub := w.hub(external, streams, grants)

	snapshotter := &control.Snapshotter{DataRoot: w.cfg.Data.Root, Runtime: w.runtime}
	mods := w.mods(snapshotter)
	ctl, err := control.New(w.cfg, &control.Deps{
		DB: w.db, Engine: w.engine, Runtime: w.runtime, Keeper: w.keeper, Streams: streams,
		Snapshotter: snapshotter, Installer: mods.install, Notifier: notifier, Commands: w.commands,
		PublishState: hub.PublishState, PublishMods: hub.PublishMods,
		ReadMods: func(ctx context.Context, inst *store.Instance) ([]store.InstanceMod, error) {
			_, installed, err := instanceDefinition(ctx, w.db, inst)
			return installed, err
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build instance control: %w", err)
	}
	instances := &Instances{
		ctl: ctl,
		alerts: &scan.Scanner{
			DB: w.db, Engine: w.engine, DataRoot: w.cfg.Data.Root, Dispatcher: notifier,
			AlarmFloor: reportedAlarmFloor(streams, w.cfg.Data.FreeSpaceFloorBytes),
		},
		DB: w.db, Authz: w.az, Runtime: w.runtime, Keeper: w.keeper, Engine: w.engine, Cfg: w.cfg,
		Streams: streams, Commands: w.commands, Mods: mods.install, Notify: notifier,
	}
	instanceRoutes(w.routes, instances)
	diagnostics := w.diagnostics(instances, mods)
	sched := w.schedules(instances, hub)
	shutdowns := w.shutdowns(instances, notifier)
	bot := w.discord(ctl.Starter, streams)

	srv := &Server{
		router: rt, health: health, supervisor: ctl.Supervisor, instances: instances, mods: mods,
		scheduler: sched, shutdowns: shutdowns, players: players, webhooks: webhooks, notifier: notifier,
		remoteBackups: remoteBackups, diagnostics: diagnostics, discord: bot, hub: hub,
	}
	w.engine.OnFinish(srv.onJobFinished)
	registerCancellationPolicies(w.engine)
	srv.routes = w.finishRouter(rt, extraRoutes, trusted, external, gate, d.Options.SPA)
	return srv, nil
}

// onJobFinished is the engine's finish hook, with two owners: the definition chain records its
// step and the notifier records what it owes, both inside the finish transaction. A
// notification never changes a job's outcome, so it is ordered last.
func (s *Server) onJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	if err := s.instances.ctl.Operations.OnJobFinished(ctx, tx, fin); err != nil {
		return err //nolint:wrapcheck // preserve the operation's original job error
	}
	return s.notifier.OnJobFinished(ctx, tx, fin) //nolint:wrapcheck // preserve the notifier's original job error
}

// accountRoutes registers the user, session, permission, audit, job and key endpoints and
// returns the grants handler, whose changes the hub announces.
func (w *wiring) accountRoutes(gate *middleware.BootstrapGate) *Grants {
	permissionRoutes(w.routes, &Permissions{Authz: w.az, DB: w.db, Commands: w.commands})
	authRoutes(w.routes, NewAuth(auth.NewBootstrap(w.db), w.sessions, gate, w.keeper))
	userRoutes(w.routes, &Users{DB: w.db, Sessions: w.sessions, Authz: w.az})
	grants := &Grants{DB: w.db, Authz: w.az}
	grantRoutes(w.routes, grants)
	auditRoutes(w.routes, &Audit{DB: w.db, Authz: w.az})
	inviteRoutes(w.routes, NewInvites(
		w.db, auth.NewInvites(w.db, w.cfg.Auth.InviteTTL.Std()), w.sessions, w.az, w.keeper,
		w.cfg.Server.ExternalURL,
	))
	jobRoutes(w.routes, &Jobs{Engine: w.engine, Authz: w.az, DB: w.db})
	keyRoutes(w.routes, &Keys{DB: w.db, Authz: w.az, Engine: w.engine, Keeper: w.keeper})
	return grants
}

// notifications builds the webhook endpoints and the notifier every domain event goes through.
// A nil sender is the production sender.
func (w *wiring) notifications(sender *notify.Sender) (*Webhooks, *delivery.Notifier) {
	if sender == nil {
		sender = &notify.Sender{}
	}
	webhooks := &Webhooks{DB: w.db, Authz: w.az, Engine: w.engine, Keeper: w.keeper, Sender: sender}
	webhooks.dispatcher = &delivery.Dispatcher{
		DB: w.db, Engine: w.engine, Keeper: w.keeper,
		Sender: func() *notify.Sender { return webhooks.Sender },
	}
	webhookRoutes(w.routes, webhooks)
	return webhooks, &delivery.Notifier{
		DB: w.db, Dispatcher: webhooks.dispatcher, ExternalURL: w.cfg.Server.ExternalURL,
	}
}

// discord builds the Discord bot and its admin endpoints.
func (w *wiring) discord(starter *control.Starter, streams *instance.Streams) *discord.Bot {
	bot := discord.New(w.db, w.keeper, starter, func(id string) *int {
		if r := streams.Reader(id); r != nil {
			return r.Players()
		}
		return nil
	})
	discordRoutes(w.routes, &Discord{DB: w.db, Authz: w.az, Keeper: w.keeper, Bot: bot})
	return bot
}

// remoteBackups builds the off-host copy endpoints and their copy worker.
func (w *wiring) remoteBackups() *RemoteBackups {
	h := &RemoteBackups{DB: w.db, Authz: w.az, Engine: w.engine, Keeper: w.keeper, Cfg: w.cfg}
	h.worker = &remotecopy.Worker{DB: w.db, Engine: w.engine, BackendFor: h.backend}
	remoteBackupRoutes(w.routes, h)
	return h
}

// hub builds the WebSocket hub and connects everything that publishes to it.
func (w *wiring) hub(external *url.URL, streams *instance.Streams, grants *Grants) *ws.Hub {
	socks := &sockets{engine: w.engine, streams: streams}
	hub := ws.New(&ws.Config{
		Origin: external.Scheme + "://" + external.Host,
		CSRF: func(sessionID, token string) bool {
			want, err := middleware.CSRFToken(w.keeper, sessionID)
			return err == nil && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
		},
		Authz: w.az,
		Res:   resolver{db: w.db},
		Src:   ws.Sources{Console: socks.console, Stats: socks.stats, Job: socks.job},
		SessionExpiry: func(ctx context.Context, sessionID string) (time.Time, error) {
			return w.db.SessionAbsoluteExpiry(ctx, sessionID)
		},
	})
	grants.Changes = hub
	// The join code is latched by the log reader, so the announcement starts there rather than
	// in a handler, while the session it names is still running.
	streams.OnJoinCode = hub.PublishJoinCode
	// A stream route, so no server-wide write deadline severs the console.
	w.routes.Stream("GET /api/v1/ws", hub)
	// A revoked session has to reach the socket it left open, not only its next request.
	w.sessions.OnRevoke(func(sessionID, userID string) {
		if sessionID != "" {
			hub.SessionRevoked(sessionID)
		}
		if userID != "" {
			hub.UserRevoked(userID)
		}
	})
	// The engine publishes a transition in the same moment it writes one.
	w.engine.Announce(announceState(w.db, hub))
	return hub
}

// mods builds the mod engine: one client and one zip cache per enabled registry, the planner,
// the installer and the registry sync. A disabled registry is absent from both maps.
func (w *wiring) mods(snapshotter *control.Snapshotter) *Mods {
	clients := map[source.Source]*thunderstore.Client{
		source.Thunderstore: thunderstore.New(w.cfg.Thunderstore.BaseURL),
	}
	if w.cfg.Hexium.Enabled {
		clients[source.Hexium] = thunderstore.NewBare(w.cfg.Hexium.BaseURL)
	}
	caches := make(map[source.Source]*cache.Cache, len(clients))
	for src := range clients {
		caches[src] = cache.New(cache.RootFor(w.cfg.Data.Root, src))
	}
	m := &Mods{DB: w.db, Authz: w.az, Engine: w.engine, Clients: clients}
	m.plan = &manager.Planner{DB: w.db, Enabled: m.enabledSources()}
	m.install = &manager.Installer{
		DB: w.db, Engine: w.engine, Commands: w.commands, Clients: clients, Caches: caches,
		DataRoot: w.cfg.Data.Root, ArchiveWorlds: snapshotter.Snapshot,
	}
	m.syncer = &manager.Syncer{
		DB: w.db, Engine: w.engine, Clients: clients, Interval: w.cfg.Thunderstore.SyncInterval.Std(),
	}
	modRoutes(w.routes, m)
	return m
}

// diagnostics builds the diagnostics endpoints, reusing the enabled registry clients for their
// live checks.
func (w *wiring) diagnostics(instances *Instances, mods *Mods) *Diagnostics {
	d := &Diagnostics{
		Instances: instances, Packages: mods.Clients[source.Thunderstore], StartedAt: time.Now().UTC(),
	}
	if client := mods.Clients[source.Hexium]; client != nil {
		d.Hexium = client
	}
	diagnosticRoutes(w.routes, d)
	return d
}

// schedules builds the schedule endpoints and the clock that runs them.
func (w *wiring) schedules(instances *Instances, hub *ws.Hub) *scheduler.Scheduler {
	schedules := &Schedules{DB: w.db, Authz: w.az, Instances: instances, Hub: hub}
	scheduleRoutes(w.routes, schedules)
	return &scheduler.Scheduler{
		DB: w.db, Interval: scheduleTickInterval, Enqueue: schedules.Enqueue,
		Occupied: schedules.Occupied, Held: schedules.announce, Warn: schedules.Warn,
	}
}

// shutdowns builds the planned power cut endpoints and the clock that acts on them.
func (w *wiring) shutdowns(instances *Instances, notifier *delivery.Notifier) *scheduler.Shutdowns {
	h := &Shutdowns{DB: w.db, Authz: w.az, Instances: instances}
	shutdownRoutes(w.routes, h)
	return &scheduler.Shutdowns{
		DB: w.db, Interval: shutdownTickInterval, Stop: h.Stop, Warn: h.Warn,
		Announce: notifier.NotifyPowerCutSoon,
	}
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

// finishRouter installs every registered route behind the API chain, then the SPA fallback,
// and returns the routes it installed. A nil spa serves the embedded build.
func (w *wiring) finishRouter(
	rt *Router, extraRoutes []routeSpec, trusted []netip.Prefix, external *url.URL,
	gate *middleware.BootstrapGate, spa http.Handler,
) []routeSpec {
	w.routes.routes = append(w.routes.routes, extraRoutes...)
	large := w.routes.install(rt)
	rt.chain = middleware.Chain(&middleware.Config{
		TrustedProxies: trusted,
		ExternalURL:    external,
		BodyLimit:      w.cfg.Server.BodyLimitBytes,
		LargeBody: func(r *http.Request) bool {
			_, pattern := rt.api.Handler(r)
			return large[pattern]
		},
		Keeper:    w.keeper,
		PerIP:     ratelimit.New(300, time.Minute, 100),
		PerUser:   ratelimit.New(300, time.Minute, 100),
		Bootstrap: gate,
		Auth:      w.sessions,
	})
	// Registering /api/ here keeps the API structural: http.ServeMux takes the most specific
	// pattern, so the SPA's "/" cannot answer a mistyped endpoint with 200 and a page of HTML.
	rt.mux.Handle("/api/", middleware.Apply(http.HandlerFunc(rt.dispatch), rt.chain))
	rt.spa = spa
	if rt.spa == nil {
		rt.spa = SPA(web.Assets)
	}
	rt.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.spa.ServeHTTP(w, r)
	}))
	return append([]routeSpec(nil), w.routes.routes...)
}
