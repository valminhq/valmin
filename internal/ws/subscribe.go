package ws

import (
	"context"
	"log/slog"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
)

// subscribe handles 14 §2.2: Can is called for every topic in every subscribe message, never
// once at connect, since a hub that trusts the client's topic list after authenticating the
// connection is a cross-user leak (D1, 09 §4.1).
//
// Acknowledgement is per topic (14 §2.3): one bad topic does not fail the others.
func (c *conn) subscribe(ctx context.Context, raw string) {
	t, ok := Parse(raw)
	if !ok {
		c.sendError(raw, apierr.InvalidParameter)
		return
	}

	c.mu.Lock()
	_, already := c.subs[t]
	count := len(c.subs)
	c.mu.Unlock()
	if already {
		return
	}
	if count >= maxTopics {
		// A count limit is an error message, not a close (14 §3.3) — the other topics on
		// this connection are still working.
		c.sendError(raw, apierr.RateLimited)
		return
	}

	instanceID, allowed := c.authorize(ctx, t)
	if !allowed {
		// not_found, not forbidden (D2, 14 §2.3). Getting this right in the REST layer
		// and wrong here leaves the enumeration oracle open on the transport that is
		// *easier* to script against.
		c.sendError(raw, apierr.NotFound)
		return
	}

	sub := &subscription{topic: t, instanceID: instanceID, stop: make(chan struct{})}
	var replay []Message
	var live <-chan Message
	if open := c.hub.source(t); open != nil {
		replay, live, sub.cancel = open(t.ID())
	}

	c.mu.Lock()
	if _, raced := c.subs[t]; raced {
		c.mu.Unlock()
		if sub.cancel != nil {
			sub.cancel()
		}
		return
	}
	c.subs[t] = sub
	c.mu.Unlock()

	// seq is the topic's current sequence number, which is what lets a client reconcile
	// replay against live messages without comparing content (14 §2.3).
	var seq uint64
	if n := len(replay); n > 0 {
		seq = replay[n-1].Seq
	}
	c.deliverRaw(t, 0, subscribedMsg{Type: "subscribed", Topic: t.String(), Seq: seq})

	if live == nil && replay == nil {
		return
	}
	go c.forward(sub, replay, live)
}

// forward moves one source's messages into the connection's queues. One goroutine per
// subscription is what lets a slow console stall only itself: the source's own channel
// backs up and drops, and the sequence discontinuity tells the client so.
func (c *conn) forward(sub *subscription, replay []Message, live <-chan Message) {
	for _, m := range replay {
		// Blocking, deliberately: replay exists for the pinned startup segment (G8), and a
		// drop-oldest push would throw away exactly those boot lines. This only waits on a client
		// that is not consuming, which the stuck timer ends.
		if !c.pushWait(sub, m) {
			return
		}
	}
	for {
		select {
		case m, ok := <-live:
			if !ok {
				return
			}
			if !c.pushSub(sub, m) {
				return
			}
		case <-sub.stop:
			return
		case <-c.done:
			return
		}
	}
}

// pushSub numbers a lossy message that arrived without a sequence of its own, so a drop is
// still visible to the client. Only this goroutine touches sub.nextSeq.
func (c *conn) pushSub(sub *subscription, m Message) bool {
	seq := m.Seq
	if seq == 0 && sub.topic.Class() == Lossy {
		sub.nextSeq++
		seq = sub.nextSeq
	}
	return c.push(sub.topic, seq, m.Payload)
}

func (c *conn) pushWait(sub *subscription, m Message) bool {
	ob := outbound{topic: sub.topic, seq: m.Seq, payload: m.Payload}
	select {
	case c.lossy <- ob:
		return true
	case <-sub.stop:
		return false
	case <-c.done:
		return false
	}
}

// authorize resolves a topic to the instance its authorization hangs on, and decides.
func (c *conn) authorize(ctx context.Context, t Topic) (instanceID string, allowed bool) {
	if t.Kind() == KindJob {
		return c.authorizeJob(ctx, t.ID())
	}
	// Existence is asked separately from permission, because Can says yes to an admin
	// for every instance id including one that does not exist — and an admin subscribed to
	// a typo waits forever for a message with no source.
	exists, err := c.hub.cfg.Res.InstanceExists(ctx, t.ID())
	if err != nil {
		slog.ErrorContext(ctx, "instance lookup failed for a subscription",
			slog.String("topic", t.String()), slog.Any("error", err))
		return "", false
	}
	if !exists {
		return "", false
	}
	return t.ID(), c.hub.cfg.Authz.Can(ctx, c.user, t.Action(), t.ID())
}

// authorizeJob resolves the job row first and decides against its instance_id, since the topic
// string carries no instance (09 §4.1).
//
// A terminal job is still subscribable: a client subscribing just after one succeeded gets the
// topic, receives nothing and reads the outcome from GET /jobs/{id}. Refusing would make
// subscribe-then-fetch unimplementable (12 §7).
func (c *conn) authorizeJob(ctx context.Context, jobID string) (instanceID string, allowed bool) {
	id, found, err := c.hub.cfg.Res.JobInstance(ctx, jobID)
	if err != nil {
		slog.ErrorContext(ctx, "job lookup failed for a subscription",
			slog.String("job_id", jobID), slog.Any("error", err))
		return "", false
	}
	if !found {
		return "", false
	}
	if id == "" {
		// A job with no instance is either global or one whose instance was deleted, 12 §4.2
		// setting the column NULL rather than cascading the row away. Both are admin-only, which is
		// what Can answers for an empty instance id (14 §2.2).
		return "", c.hub.cfg.Authz.Can(ctx, c.user, authz.InstanceView, "")
	}
	return id, c.hub.cfg.Authz.Can(ctx, c.user, authz.InstanceView, id)
}

// source picks the Subscribe for a topic kind. State has none: 14 §4.4's writers publish it
// (PublishState), so there is nothing to read from.
func (h *Hub) source(t Topic) Subscribe {
	switch t.Kind() {
	case KindConsole:
		return h.cfg.Src.Console
	case KindStats:
		return h.cfg.Src.Stats
	case KindJob:
		return h.cfg.Src.Job
	case KindState:
		return nil
	default:
		return nil
	}
}

func (c *conn) unsubscribe(raw string) {
	t, ok := Parse(raw)
	if !ok {
		c.sendError(raw, apierr.InvalidParameter)
		return
	}
	c.drop(t)
}

// drop releases one subscription. It does not notify the client, which asked for this.
func (c *conn) drop(t Topic) *subscription {
	c.mu.Lock()
	sub := c.subs[t]
	delete(c.subs, t)
	c.mu.Unlock()
	if sub == nil {
		return nil
	}
	close(sub.stop)
	if sub.cancel != nil {
		sub.cancel()
	}
	return sub
}

func (c *conn) unsubscribeAll() {
	c.mu.Lock()
	subs := make([]*subscription, 0, len(c.subs))
	for t, sub := range c.subs {
		subs = append(subs, sub)
		delete(c.subs, t)
	}
	c.mu.Unlock()
	for _, sub := range subs {
		close(sub.stop)
		if sub.cancel != nil {
			sub.cancel()
		}
	}
}

// recheck re-asks Can for every topic hanging on instanceID and drops the ones that no longer
// pass (14 §6). The connection survives, since the user may still see other instances, and a
// dropped topic gets forbidden rather than not_found: this user could see it a moment ago.
func (c *conn) recheck(ctx context.Context, instanceID string) {
	c.mu.Lock()
	affected := make([]*subscription, 0, len(c.subs))
	for _, sub := range c.subs {
		if sub.instanceID == instanceID {
			affected = append(affected, sub)
		}
	}
	c.mu.Unlock()

	for _, sub := range affected {
		act := sub.topic.Action()
		if sub.topic.Kind() == KindJob {
			act = authz.InstanceView
		}
		if c.hub.cfg.Authz.Can(ctx, c.user, act, instanceID) {
			continue
		}
		if c.drop(sub.topic) != nil {
			c.sendError(sub.topic.String(), apierr.Forbidden)
		}
	}
}

// dropInstance ends every subscription hanging on one instance — 14 §6's deleted-instance
// row, which drops topics and closes nothing.
func (c *conn) dropInstance(instanceID string, code apierr.Code) {
	c.mu.Lock()
	topics := make([]Topic, 0, len(c.subs))
	for t, sub := range c.subs {
		if sub.instanceID == instanceID {
			topics = append(topics, t)
		}
	}
	c.mu.Unlock()

	for _, t := range topics {
		if c.drop(t) != nil {
			c.sendError(t.String(), code)
		}
	}
}

// command is 14 §7.3. The hub does not execute commands: it authorizes commands.send and
// delegates to the command channel provider, which resolves to none on this build (07 §5, E3).
//
// The answer is unsupported rather than silence, so 07 §4's stdin probe can change it without
// touching the protocol.
func (c *conn) command(ctx context.Context, instanceID string) {
	if instanceID == "" || !c.hub.cfg.Authz.Can(ctx, c.user, authz.CommandsSend, instanceID) {
		c.sendError("", apierr.NotFound)
		return
	}
	c.sendError("", apierr.Unsupported)
}
