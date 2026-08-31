package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/store"
)

// outbound is one queued frame. The topic travels with it so the writer can report a gap
// against the right sequence without inspecting the payload.
type outbound struct {
	topic   Topic
	seq     uint64
	payload any
}

// subscription is one live topic on one connection.
type subscription struct {
	topic Topic
	// instanceID is the instance this topic's authorization hangs on — the topic's own id
	// for the instance topics, and the resolved one for a job. It is kept so revocation can
	// re-ask Can without resolving the job row a second time (14 §6).
	instanceID string
	cancel     func()
	stop       chan struct{}
	// nextSeq numbers a lossy source that carries no sequence of its own, so a drop is
	// still visible to the client as a discontinuity.
	nextSeq uint64
}

// conn is one browser session's socket.
type conn struct {
	hub         *Hub
	ws          *websocket.Conn
	user        *store.User
	sessionID   string
	connectedAt time.Time

	// `↯` Two queues, not one (ADR-039). A single queue with drop-oldest would eventually
	// drop a state message to make room for a console line, which is the exact trade the
	// lossy/lossless split exists to forbid.
	lossy    chan outbound
	lossless chan outbound

	done      chan struct{}
	closeOnce sync.Once
	// closedWith is why this connection ended, which is the one thing about it worth
	// keeping after it has gone: 4401 and 4403 mean different things to the client, and so
	// do 1008 and 1011.
	closedWith websocket.StatusCode

	mu         sync.Mutex
	subs       map[Topic]*subscription
	stuckSince time.Time
	csrfDone   bool
}

func newConn(h *Hub, sock *websocket.Conn, u *store.User, sessionID string, csrfDone bool) *conn {
	return &conn{
		hub: h, ws: sock, user: u, sessionID: sessionID, connectedAt: time.Now(),
		lossy:    make(chan outbound, queueDepth),
		lossless: make(chan outbound, queueDepth),
		done:     make(chan struct{}),
		subs:     make(map[Topic]*subscription),
		csrfDone: csrfDone,
	}
}

// serve runs the connection until it closes.
func (c *conn) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.unsubscribeAll()

	// `↯` ctx is deliberately *not* cancelled when the connection is closed from elsewhere
	// — revocation, the expiry timer, shutdown. coder/websocket aborts the underlying
	// connection when a read's context is cancelled, so cancelling here races the closing
	// exchange and the peer gets an EOF instead of the code. A client that cannot read
	// 4401 from 4403 cannot tell "sign in again" from "an admin narrowed a grant", which
	// is the whole reason 14 §3.4 gives them separate numbers. The read loop ends on its
	// own when Close lands.

	// `↯` D16: absolute expiry needs a timer, not a check. A socket open for twelve hours
	// makes no requests, and 10 §4.1's absolute expiry is only ever noticed on one — so
	// without this, "sessions expire after N hours" is false for exactly the connection
	// that matters most.
	if c.hub.cfg.SessionExpiry != nil {
		until, err := c.hub.cfg.SessionExpiry(ctx, c.sessionID)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "session expiry unreadable; closing rather than guessing",
				slog.String("session_id", c.sessionID), slog.Any("error", err))
			c.close(statusSessionExpired, "this session could not be verified")
			return
		case !until.IsZero():
			timer := time.AfterFunc(time.Until(until), func() {
				c.close(statusSessionExpired, "this session has expired")
			})
			defer timer.Stop()
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writeLoop(ctx) }()
	go func() { defer wg.Done(); c.pingLoop(ctx) }()

	c.readLoop(ctx)
	c.close(websocket.StatusNormalClosure, "")
	wg.Wait()
}

// close is the one exit. It is idempotent: revocation, the expiry timer, the read loop and
// shutdown can all reach it, and the first one wins.
func (c *conn) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closedWith = code
		c.mu.Unlock()
		close(c.done)
		if c.ws == nil {
			return
		}
		// `↯` Off the caller's goroutine. Close writes the close frame and then waits for
		// the peer's reply, and this connection's own read loop is what consumes that — so
		// the wait always runs out its five seconds. Blocking here would make shutdown five
		// seconds per open socket, serially, and would stall a revocation behind the tab it
		// is revoking. The frame goes out immediately either way, which is the part the
		// client needs.
		go func() { _ = c.ws.Close(code, reason) }()
	})
}

func (c *conn) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m clientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			c.sendError("", apierr.MalformedJSON)
			continue
		}
		// 11 §6.3: the token comes as a query parameter or the first message. A first
		// message that is not it is a client that cannot prove it is the page it claims to
		// be, and there is nothing to negotiate.
		if !c.csrfDone {
			if !c.hub.csrfOK(c.sessionID, m.CSRF) {
				c.close(websocket.StatusPolicyViolation, "csrf token missing or wrong")
				return
			}
			c.csrfDone = true
		}

		c.dispatch(ctx, &m)
	}
}

// dispatch is the whole client→server surface (04 §4). `↯` Nothing here performs work: a
// new type that did would be a REST endpoint wearing a disguise, and it would get neither
// audit logging nor the error envelope for free (14 §9).
func (c *conn) dispatch(ctx context.Context, m *clientMsg) {
	switch m.Type {
	case "subscribe":
		for _, raw := range m.Topics {
			if !c.allowChurn(raw) {
				return
			}
			c.subscribe(ctx, raw)
		}
	case "unsubscribe":
		for _, raw := range m.Topics {
			if !c.allowChurn(raw) {
				return
			}
			c.unsubscribe(raw)
		}
	case "ping":
		// Redundant with the protocol ping and worth the four lines: some browser stacks
		// make protocol pongs invisible to page JavaScript, and a client that wants to
		// prove its own liveness should be able to (14 §3.2).
		c.deliverRaw(Topic{}, 0, pongMsg{Type: "pong"})
	case "console.command":
		c.command(ctx, m.Instance)
	default:
		c.sendError("", apierr.InvalidParameter)
	}
}

func (c *conn) allowChurn(topic string) bool {
	if ok, _ := c.hub.churn.Allow(c.sessionID); ok {
		return true
	}
	c.sendError(topic, apierr.RateLimited)
	return false
}

// pingLoop is 14 §3.2's keepalive. Ping blocks until the pong arrives, so the deadline is
// the whole check.
func (c *conn) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pongTimeout)
		err := c.ws.Ping(pctx)
		cancel()
		if err != nil {
			c.close(websocket.StatusGoingAway, "no pong")
			return
		}
	}
}

// writeLoop is the only goroutine that writes to the socket (C21). Fan-out never happens on
// a source's goroutine: one wedged TCP connection must not be able to reach back and stall
// a Docker log stream, which is what "just write to all subscribers" does, and it presents
// as *every* console freezing when *one* user's laptop sleeps.
func (c *conn) writeLoop(ctx context.Context) {
	last := make(map[Topic]uint64)
	for {
		var ob outbound
		// Lossless first: a state transition waiting behind a thousand console lines is a
		// dashboard that is wrong for as long as the backlog takes to drain.
		select {
		case ob = <-c.lossless:
		default:
			select {
			case ob = <-c.lossless:
			case ob = <-c.lossy:
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
		}

		if g, ok := gapBefore(last, ob); ok {
			// The gap is reported where the client needs it — in order, immediately before
			// the first message that follows the break — rather than as a seamless lie
			// (ADR-039).
			if !c.write(ctx, g) {
				return
			}
		}
		if !c.write(ctx, ob.payload) {
			return
		}
	}
}

func (c *conn) write(ctx context.Context, payload any) bool {
	wctx, cancel := context.WithTimeout(ctx, pongTimeout)
	defer cancel()
	if err := wsjson.Write(wctx, c.ws, payload); err != nil {
		c.close(websocket.StatusInternalError, "write failed")
		return false
	}
	return true
}

// deliver queues one message for a topic this connection actually holds. It is what
// PublishState calls, and it is a no-op for a connection that never subscribed.
func (c *conn) deliver(t Topic, m Message) {
	c.mu.Lock()
	_, subscribed := c.subs[t]
	c.mu.Unlock()
	if subscribed {
		c.push(t, m.Seq, m.Payload)
	}
}

// deliverRaw queues a frame that belongs to no topic — an acknowledgement, an error, a
// pong. These are never dropped: they are the connection talking about itself.
func (c *conn) deliverRaw(t Topic, seq uint64, payload any) {
	select {
	case c.lossless <- outbound{topic: t, seq: seq, payload: payload}:
	case <-c.done:
	}
}

func (c *conn) sendError(topic string, code apierr.Code) {
	c.deliverRaw(Topic{}, 0, errorMsg{
		Type: "error", Topic: topic, Code: code.String(), Message: code.Message(),
	})
}

// push enqueues one message under its topic's backpressure class (14 §5).
func (c *conn) push(t Topic, seq uint64, payload any) bool {
	ob := outbound{topic: t, seq: seq, payload: payload}
	if t.Class() == Lossless {
		select {
		case c.lossless <- ob:
			return true
		case <-c.done:
			return false
		default:
		}
		// `↯` Closing is the safe failure, not the harsh one. The client reconnects and
		// re-syncs from REST (14 §7.2), which is a second of ugliness; a client that
		// silently missed `state: stopped` shows a running server that is not, and the
		// operator acts on it.
		c.close(websocket.StatusInternalError, "the client is not consuming state updates")
		return false
	}

	select {
	case c.lossy <- ob:
		c.mu.Lock()
		c.stuckSince = time.Time{}
		c.mu.Unlock()
		return true
	case <-c.done:
		return false
	default:
	}

	if c.stuckTooLong() {
		c.close(websocket.StatusInternalError, "the client stopped consuming")
		return false
	}
	// Drop the oldest and take its place. The client learns from the sequence
	// discontinuity, which the writer turns into a visible break.
	select {
	case <-c.lossy:
	default:
	}
	select {
	case c.lossy <- ob:
	default:
	}
	return true
}

// stuckTooLong reports whether the lossy queues have been full for 14 §5's 30 seconds. At
// that point the client is not consuming and holding buffers for it helps nobody.
func (c *conn) stuckTooLong() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stuckSince.IsZero() {
		c.stuckSince = time.Now()
		return false
	}
	return time.Since(c.stuckSince) > stuckTimeout
}

// gapBefore reports the break to announce before ob, and records ob's place. A
// discontinuity is the only evidence of a drop the client ever gets, and it is enough:
// whether the message was lost in the source, in the adapter or in this connection's own
// queue, the client renders one visible break and re-syncs from REST if it cares (14 §7.2).
func gapBefore(last map[Topic]uint64, ob outbound) (g gapMsg, ok bool) {
	if ob.seq == 0 {
		return gapMsg{}, false
	}
	seen, seenBefore := last[ob.topic]
	last[ob.topic] = ob.seq
	if !seenBefore || ob.seq <= seen+1 {
		return gapMsg{}, false
	}
	dropped := ob.seq - seen - 1
	if dropped > math.MaxInt32 {
		// Unreachable with a queue of 256 and a source that counts lines, but a wrapped
		// negative "dropped" would render as a nonsense gap rather than a visible break.
		dropped = math.MaxInt32
	}
	return gapMsg{
		Type: "gap", Topic: ob.topic.String(),
		Dropped: int(dropped), FromSeq: seen + 1,
	}, true
}

// closeReason reports the code this connection ended with, or 0 while it is still open.
func (c *conn) closeReason() websocket.StatusCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closedWith
}
