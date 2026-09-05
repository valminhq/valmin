// Package ws is the WebSocket hub: topics, per-topic authorization and fan-out.
//
// Specification: 14. It is a transport and holds no game-specific knowledge (ADR-042) —
// framing, line reassembly and pattern matching live in internal/instance, next to the
// measured log grammar they depend on, and the adapters that turn a log line into a wire
// message live in internal/api. This package imports neither.
//
// Nor is it a system of record (14 §9): job state is in job_runs, instance state in
// instances, and the audit trail in audit_log. It interprets no roles — it calls Can, per
// topic, on every subscribe.
package ws

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// Limits from 14 §3.3.
const (
	// maxClientMessage is the client→server cap. Nothing legitimate is larger: the biggest
	// thing a client sends is a console command, which is a line of text.
	maxClientMessage = 4 << 10
	// maxTopics is what one connection may hold. A dashboard needs a few dozen at most.
	maxTopics = 64
	// maxConnectionsPerUser is tabs, a phone, and a stale one. The ninth closes the oldest.
	maxConnectionsPerUser = 8
	// queueDepth is ADR-039's bounded per-connection queue.
	queueDepth = 256
)

// Keepalive (14 §3.2), sized under nginx's 60 s default proxy_read_timeout: once the upgrade
// completes the proxy is tunnelling frames, and a server with nobody online logs nothing for
// minutes, so an idle tunnel would be closed under the console.
const (
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
)

// stuckTimeout is 14 §5's last resort: a connection whose lossy queues stay full this long is
// closed, since the client is not consuming. A var so a test need not wait it out.
var stuckTimeout = 30 * time.Second

// Close codes beyond the RFC's own (14 §3.4). The two are distinct on purpose: one
// means *you are no longer logged in*, the other means *you are, but not to this*. A client
// that conflates them logs the user out because an admin narrowed one grant.
const (
	statusSessionExpired websocket.StatusCode = 4401
	statusAccessRevoked  websocket.StatusCode = 4403
)

// Authorizer is the one authorization function (09 §4), as the hub needs it. Defined here,
// the consumer, per 06 §4.
type Authorizer interface {
	Can(ctx context.Context, u *store.User, act authz.Action, instanceID string) bool
}

// Resolver answers the two existence questions a topic raises before it can be authorized.
// InstanceExists is not redundant with Can: an admin is allowed every instance, including one
// that does not exist, and would otherwise subscribe happily to a typo.
type Resolver interface {
	InstanceExists(ctx context.Context, instanceID string) (bool, error)
	// JobInstance resolves a job to the instance it belongs to. found is false for an
	// unknown job; an empty instanceID on a found job is a global job, which is admin-only
	// (14 §2.2) and also covers a job whose instance has since been deleted.
	JobInstance(ctx context.Context, jobID string) (instanceID string, found bool, err error)
}

// Subscribe opens one source of messages for id. replay is the backlog a late-joining
// browser gets before live messages start, cancel releases the subscription, and the
// channel is closed by neither — the hub calls cancel.
type Subscribe func(id string) (replay []Message, live <-chan Message, cancel func())

// Sources are the streams the hub fans out, supplied as functions rather than an interface so
// the hub does not know what produces them (ADR-042).
//
// State is deliberately absent: it has no stream, no replay and no per-instance lifecycle, and
// its two writers publish it as they write it (14 §4.4), so the hub takes it as PublishState.
type Sources struct {
	Console Subscribe
	Stats   Subscribe
	Job     Subscribe
}

// Config is everything the hub needs to stand up.
type Config struct {
	// Origin is the scheme://host every upgrade must carry (11 §6.3).
	Origin string
	// CSRF verifies the double-submit token against a session. The hub does not need to
	// know how it is derived, and taking it as a function keeps 10 §3's key material out of
	// a package that only ever compares two strings.
	CSRF  func(sessionID, token string) bool
	Authz Authorizer
	Res   Resolver
	Src   Sources
	// SessionExpiry reports when a session's absolute expiry falls. The zero time arms no
	// timer, which is what a test without a session table wants.
	SessionExpiry func(ctx context.Context, sessionID string) (time.Time, error)
}

// Hub is the WebSocket surface: one connection per browser session, per-topic authorization on
// every subscribe, and fan-out that never blocks a source. It holds no lifecycle of its own
// (14 §8) and is not a system of record (14 §9).
type Hub struct {
	cfg   *Config
	churn *middleware.Limiter

	mu     sync.Mutex
	conns  map[*conn]struct{}
	closed bool
}

// New builds a hub. Nothing starts until a client connects.
func New(cfg *Config) *Hub {
	return &Hub{
		cfg: cfg,
		// 14 §3.3: churn is the cheap attack on a hub that authorizes per subscribe, so the
		// message rate is capped per session rather than per connection — eight tabs of one
		// session share one budget.
		churn: middleware.NewLimiter(600, time.Minute, 120),
		conns: make(map[*conn]struct{}),
	}
}

// ServeHTTP is the upgrade endpoint, GET /api/v1/ws. Every rejection answers in the normal error
// envelope before the upgrade, so a refused client reads a response rather than a close frame
// (11 §6.3).
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := middleware.UserFrom(ctx)
	sessionID := middleware.SessionIDFrom(ctx)
	if u == nil || sessionID == "" {
		apierr.Write(w, r, apierr.New(apierr.Unauthenticated))
		return
	}

	// Deliberately the opposite of the REST rule (11 §6.3): an upgrade is not subject to the
	// same-origin policy, triggers no preflight and carries cookies. Browsers always send Origin
	// on one, so a missing header is a hijack shape rather than a curl user.
	if origin := r.Header.Get("Origin"); origin != h.cfg.Origin {
		apierr.Write(w, r, apierr.New(apierr.OriginRejected))
		return
	}

	// The CSRF token arrives as a query parameter, because a browser cannot set a header on
	// the WebSocket constructor. 04 §4's first-message form is accepted too, in readMessage.
	if token := r.URL.Query().Get("csrf"); token != "" {
		if !h.csrfOK(sessionID, token) {
			apierr.Write(w, r, apierr.New(apierr.CSRFFailed))
			return
		}
	}
	csrfDone := r.URL.Query().Get("csrf") != ""

	sock, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin is checked above, against the panel's configured external URL rather than
		// the Host header the caller supplied.
		InsecureSkipVerify: true,
		// Off (14 §3.1). Console lines compress well and this is the one place it would
		// pay, but compression state is per connection and interacts badly with the drop
		// policy of §5. Revisit with a measurement, not a hunch.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		slog.WarnContext(ctx, "websocket upgrade failed", slog.Any("error", err))
		return
	}
	sock.SetReadLimit(maxClientMessage)

	c := newConn(h, sock, u, sessionID, csrfDone)
	h.add(c)
	defer h.remove(c)
	c.serve(context.WithoutCancel(ctx))
}

func (h *Hub) csrfOK(sessionID, token string) bool {
	return h.cfg.CSRF != nil && h.cfg.CSRF(sessionID, token)
}

// add registers a connection and enforces 14 §3.3's per-user cap by closing the oldest.
func (h *Hub) add(c *conn) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.close(websocket.StatusGoingAway, "the panel is shutting down")
		return
	}
	h.conns[c] = struct{}{}

	var oldest *conn
	mine := 0
	for other := range h.conns {
		if other.user.ID != c.user.ID {
			continue
		}
		mine++
		if oldest == nil || other.connectedAt.Before(oldest.connectedAt) {
			oldest = other
		}
	}
	h.mu.Unlock()

	if mine > maxConnectionsPerUser && oldest != nil {
		oldest.close(websocket.StatusPolicyViolation, "too many connections for this account")
	}
}

func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// snapshot copies the live connections so revocation and publishing never hold the hub lock
// while touching a socket.
func (h *Hub) snapshot() []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

// PublishState announces a transition, as the job engine and the observer do in the moment they
// write one (14 §4.4). Call it after the transaction commits: state is a lossless topic, so a
// transition announced from inside one could roll back with no distinguishable correction.
func (h *Hub) PublishState(instanceID, state string, restartRequired bool) {
	t := StateTopic(instanceID)
	msg := Message{Payload: StateMsg{
		Type: "state", Instance: instanceID, State: state, RestartRequired: restartRequired,
	}}
	for _, c := range h.snapshot() {
		c.deliver(t, msg)
	}
}

// GrantChanged drops the topics a revoked or narrowed grant covered, leaving the connection open
// since the user may still see other instances (14 §6). It re-asks Can rather than assuming what
// changed, so a narrowing drops exactly the topics it removes.
func (h *Hub) GrantChanged(ctx context.Context, userID, instanceID string) {
	for _, c := range h.snapshot() {
		if c.user.ID != userID {
			continue
		}
		c.recheck(ctx, instanceID)
	}
}

// InstanceDeleted drops an instance's topics on every connection and closes none (14 §6).
func (h *Hub) InstanceDeleted(instanceID string) {
	for _, c := range h.snapshot() {
		c.dropInstance(instanceID, apierr.NotFound)
	}
}

// SessionRevoked closes the connections of one session: an explicit logout, or a session
// row deleted out from under it (10 §4.1).
func (h *Hub) SessionRevoked(sessionID string) {
	for _, c := range h.snapshot() {
		if c.sessionID == sessionID {
			c.close(statusAccessRevoked, "this session was signed out")
		}
	}
}

// UserRevoked closes every connection of a user: disabled, role changed, password changed.
// 10 §4.1 deletes the session rows; this is the live half of the same act.
func (h *Hub) UserRevoked(userID string) {
	for _, c := range h.snapshot() {
		if c.user.ID == userID {
			c.close(statusAccessRevoked, "this account's access changed")
		}
	}
}

// Close closes every connection with 1001 so the SPA reconnects quietly (11 §10). It runs before
// http.Server.Shutdown, which would otherwise wait out the grace period on handlers that never
// return on their own.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	for _, c := range h.snapshot() {
		c.close(websocket.StatusGoingAway, "the panel is shutting down")
	}
}

// ConsoleTopic, StatsTopic, StateTopic and JobTopic mint a topic without going through the
// wire form, for the publishers and adapters that already hold the id.
func ConsoleTopic(instanceID string) Topic { return Topic{kind: KindConsole, id: instanceID} }
func StatsTopic(instanceID string) Topic   { return Topic{kind: KindStats, id: instanceID} }
func StateTopic(instanceID string) Topic   { return Topic{kind: KindState, id: instanceID} }
func JobTopic(jobID string) Topic          { return Topic{kind: KindJob, id: jobID} }

// Reset is the stream.reset message a source emits when its log reader restarted. The
// client clears its view rather than splicing; sequence numbers do not restart with it
// (14 §4.2).
func Reset(t Topic) Message {
	return Message{Payload: resetMsg{Type: "stream.reset", Topic: t.String()}}
}
