package ws

import (
	"context"
	"log/slog"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
)

// subscribe is 14 §2.2, and the reason this file exists on its own: Can is called for
// every topic in every subscribe message, never once at connect. A hub that authenticates
// the connection and then trusts the client's topic list is a silent cross-user leak the
// day a second user exists (D1, 09 §4.1).
//
// Acknowledgement is per topic (14 §2.3): one bad topic in a list of ten does not fail the
// other nine.
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
		// Blocking, deliberately. The pinned startup segment is the point of replay (G8),
		// and pushing 1500 lines through a 256-slot queue with drop-oldest would deliver
		// the tail and throw away exactly the boot lines an operator opened the console to
		// read. The writer drains to a socket, so this only waits on a client that is not
		// consuming — which the stuck timer already ends.
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

// authorizeJob is the subtle one 09 §4.1 warns about: the topic string carries no instance,
// so the job row is resolved first and the decision is made against its instance_id.
//
// A terminal job is still subscribable — the client that subscribes a moment after the job
// succeeded gets the topic, receives nothing, and reads the outcome from GET /jobs/{id}.
// Refusing would make 12 §7's subscribe-then-fetch ordering unimplementable.
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
		// A job with no instance is global (thunderstore_sync, prune, key_rotate) or one
		// whose instance was deleted — 12 §4.2 sets the column NULL rather than cascading
		// the row away. Either way it is admin-only, and an empty instance id is exactly
		// the question Can answers false for every member (14 §2.2).
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

// recheck is 14 §6's grant row: re-ask Can for every topic hanging on instanceID and drop
// the ones that no longer pass. The connection survives — the user may still see other
// instances — and each dropped topic gets forbidden rather than not_found, because this
// user demonstrably could see it a moment ago.
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
// delegates to the command channel provider, which on this build resolves to none (07 §5,
// E3, and 03 §7 measured zero reads on fd 0).
//
// The answer is unsupported rather than silence. The stdin probe of 07 §4 is what will
// change this answer if a future build starts reading stdin, and it will do so without
// touching the protocol — which is the whole reason the message type is reserved now.
func (c *conn) command(ctx context.Context, instanceID string) {
	if instanceID == "" || !c.hub.cfg.Authz.Can(ctx, c.user, authz.CommandsSend, instanceID) {
		c.sendError("", apierr.NotFound)
		return
	}
	c.sendError("", apierr.Unsupported)
}
