package ws

import (
	"context"
	"log/slog"

	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
)

// subscribe handles 14 §2.2: Can is called for every topic in every subscribe message, never
// once at connect, since a hub that trusts the client's topic list after authenticating the
// connection is a cross-user leak (D1, 09 §4.1).
//
// Acknowledgement is per topic (14 §2.3): one bad topic does not fail the others.
func (c *conn) subscribe(ctx context.Context, raw string) {
	t, ok := Parse(raw)
	if !ok {
		c.sendError(raw, errcode.InvalidParameter)
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
		c.sendError(raw, errcode.RateLimited)
		return
	}

	c.mu.Lock()
	gen := c.grantGen
	c.mu.Unlock()
	instanceID, allowed := c.authorize(ctx, t)
	if !allowed {
		// not_found, not forbidden (D2, 14 §2.3). Getting this right in the REST layer
		// and wrong here leaves the enumeration oracle open on the transport that is
		// *easier* to script against.
		c.sendError(raw, errcode.NotFound)
		return
	}

	sub := &subscription{topic: t, instanceID: instanceID}
	c.mu.Lock()
	if _, raced := c.subs[t]; raced {
		c.mu.Unlock()
		return
	}
	c.subs[t] = sub
	recheckMissed := c.grantGen != gen
	c.mu.Unlock()

	// A grant change between authorize and register re-checked a set this topic was not in yet.
	if recheckMissed {
		if _, still := c.authorize(ctx, t); !still {
			if c.drop(t) != nil {
				c.sendError(raw, errcode.NotFound)
			}
			return
		}
	}

	// Live messages wait on sub.mu until the acknowledgement and the replay are queued ahead
	// of them. Nothing in here waits on the database or the client, so a source is held up
	// for no longer than its own Subscribe takes.
	sub.mu.Lock()
	var replay []Message
	if open := c.hub.source(t); open != nil {
		replay, sub.cancel = open(t.ID(), func(m Message) { c.sendLive(sub, m) })
	}
	if sub.dropped {
		// Dropped while the source opened, before there was a cancel to call.
		cancel := sub.cancel
		sub.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	// seq is the topic's current sequence number, which is what lets a client reconcile
	// replay against live messages without comparing content (14 §2.3).
	if n := len(replay); n > 0 {
		sub.replayed = replay[n-1].Seq
	}
	ack := outbound{topic: t, payload: subscribedMsg{Type: "subscribed", Topic: t.String(), Seq: sub.replayed}}
	// The replay is queued whole, as one lossless frame: it exists for the pinned startup
	// segment (G8), and a drop-oldest push would throw away exactly those boot lines.
	if c.pushLossless(ack) && len(replay) > 0 {
		c.pushLossless(outbound{topic: t, replay: replay})
	}
	sub.mu.Unlock()
}

// sendLive queues one live message from a source under its topic's backpressure class. It runs
// on the source's goroutine and never waits on the client. A lossy message without a sequence
// of its own is numbered, so a drop is still visible to the client.
func (c *conn) sendLive(sub *subscription, m Message) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.dropped || (m.Seq != 0 && m.Seq <= sub.replayed) {
		return
	}
	seq := m.Seq
	if seq == 0 && sub.topic.Class() == Lossy {
		sub.nextSeq++
		seq = sub.nextSeq
	}
	c.push(sub.topic, seq, m.Payload)
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
		c.sendError(raw, errcode.InvalidParameter)
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
	sub.end()
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
		sub.end()
	}
}

// recheck re-asks Can for every topic hanging on instanceID and drops the ones that no longer
// pass (14 §6). The connection survives, since the user may still see other instances, and a
// dropped topic gets forbidden rather than not_found: this user could see it a moment ago.
func (c *conn) recheck(ctx context.Context, instanceID string) {
	c.mu.Lock()
	c.grantGen++
	affected := make([]*subscription, 0, len(c.subs))
	for _, sub := range c.subs {
		if sub.instanceID == instanceID {
			affected = append(affected, sub)
		}
	}
	c.mu.Unlock()

	for _, sub := range affected {
		if c.hub.cfg.Authz.Can(ctx, c.user, sub.topic.Action(), instanceID) {
			continue
		}
		if c.drop(sub.topic) != nil {
			c.sendError(sub.topic.String(), errcode.Forbidden)
		}
	}
}

// dropInstance ends every subscription hanging on one instance — 14 §6's deleted-instance
// row, which drops topics and closes nothing.
func (c *conn) dropInstance(instanceID string, code errcode.Code) {
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
		c.sendError("", errcode.NotFound)
		return
	}
	c.sendError("", errcode.Unsupported)
}
