package ws

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/valminhq/valmin/internal/store"
)

// bare builds a connection with no socket and no writer, so the queues can be filled
// deterministically. Everything a real connection does to them, it does through push.
func bare() *conn {
	return newConn(New(&Config{}), nil, &store.User{ID: "u"}, "s", true)
}

// TestLossyQueueDropsOldestAndTheClientSeesAGap is ADR-039's lossy half. `↯` The console
// keeps the *newest* lines, because the operator watching a server misbehave needs what it
// is doing now, not what it was doing when their laptop went to sleep.
func TestLossyQueueDropsOldestAndTheClientSeesAGap(t *testing.T) {
	c := bare()
	topic := ConsoleTopic(instA)

	const sent = queueDepth + 100
	for seq := uint64(1); seq <= sent; seq++ {
		if !c.push(topic, seq, ConsoleMsg{Line: "x"}) {
			t.Fatalf("push %d closed the connection; a lossy topic must not", seq)
		}
	}
	if len(c.lossy) != queueDepth {
		t.Fatalf("queue holds %d, want %d", len(c.lossy), queueDepth)
	}

	first := <-c.lossy
	if first.seq != sent-queueDepth+1 {
		t.Errorf("oldest surviving message has seq %d, want %d", first.seq, sent-queueDepth+1)
	}

	// What the client makes of it: the writer had already delivered seq 1, so the jump to
	// the first survivor is the whole break, named exactly.
	last := map[Topic]uint64{topic: 1}
	g, ok := gapBefore(last, first)
	if !ok {
		t.Fatal("no gap was reported for a break of a hundred messages")
	}
	if g.Dropped != int(first.seq-2) || g.FromSeq != 2 {
		t.Errorf("gap = %+v, want dropped %d from_seq 2", g, first.seq-2)
	}
	if g.Topic != topic.String() {
		t.Errorf("gap names topic %q", g.Topic)
	}

	// And an unbroken run reports nothing, or every console would render as one long tear.
	if _, ok := gapBefore(last, outbound{topic: topic, seq: first.seq + 1}); ok {
		t.Error("a contiguous message was reported as a gap")
	}
}

// TestLosslessQueueClosesRatherThanDrop is the asymmetry that makes ADR-039 worth writing
// down. `↯` A client that silently misses `state: stopped` shows a running server that is
// not, and the operator acts on it. Closing is a second of ugliness — the client reconnects
// and re-syncs from REST (14 §7.2) — and it is the safe failure.
func TestLosslessQueueClosesRatherThanDrop(t *testing.T) {
	c := bare()
	topic := StateTopic(instA)

	for i := range queueDepth {
		if !c.push(topic, 0, StateMsg{State: "running"}) {
			t.Fatalf("push %d closed the connection early", i)
		}
	}
	if c.push(topic, 0, StateMsg{State: "stopped"}) {
		t.Fatal("a state message was dropped to make room; it must close instead")
	}
	if got := c.closeReason(); got != websocket.StatusInternalError {
		t.Errorf("close code = %v, want 1011", got)
	}
	if len(c.lossy) != 0 {
		t.Error("a lossless message was queued on the lossy side")
	}
}

// TestAStuckConnectionIsEventuallyClosed is 14 §5's last resort: at that point the client
// is not consuming and holding buffers for it helps nobody.
func TestAStuckConnectionIsEventuallyClosed(t *testing.T) {
	old := stuckTimeout
	stuckTimeout = 50 * time.Millisecond
	defer func() { stuckTimeout = old }()

	c := bare()
	topic := ConsoleTopic(instA)
	for seq := uint64(1); seq <= queueDepth+1; seq++ {
		c.push(topic, seq, ConsoleMsg{})
	}
	if c.closeReason() != 0 {
		t.Fatal("the connection closed on the first drop; a burst is not a stuck client")
	}

	time.Sleep(stuckTimeout * 2)
	if c.push(topic, 9999, ConsoleMsg{}) {
		t.Fatal("a connection full for longer than the timeout was not closed")
	}
	if got := c.closeReason(); got != websocket.StatusInternalError {
		t.Errorf("close code = %v, want 1011 — the client should reconnect", got)
	}
}

// TestOneStuckSubscriberDoesNotStallAnother is C21 end to end: one wedged connection must
// not reach back and stall a Docker log stream, which is what "just write to all
// subscribers" does — and it presents as *every* console freezing when *one* user's laptop
// sleeps.
func TestOneStuckSubscriberDoesNotStallAnother(t *testing.T) {
	lines := make(chan Message)
	e := newEnv(t, &Config{
		Res: fakeRes{instances: map[string]bool{instA: true}},
		Src: Sources{Console: func(string) ([]Message, <-chan Message, func()) {
			return nil, lines, func() {}
		}},
	})

	stuck := e.dial(t, "s1")
	healthy := e.dial(t, "s2")
	topic := "instance." + instA + ".console"
	subscribe(t, stuck, topic)
	subscribe(t, healthy, topic)
	if f := read(t, healthy); f["type"] != "subscribed" {
		t.Fatalf("healthy client was not subscribed: %v", f)
	}
	// stuck deliberately never reads again, including its own acknowledgement.

	deadline := time.After(20 * time.Second)
	for seq := uint64(1); seq <= queueDepth*3; seq++ {
		select {
		case lines <- Message{Seq: seq, Payload: ConsoleMsg{Type: "console", Seq: seq, Line: "l"}}:
		case <-deadline:
			t.Fatalf("the source blocked at line %d: one sleeping client froze the stream", seq)
		}
	}

	// And the healthy one is still being served, which is the other half of the claim.
	if f := readUntil(t, healthy, "console"); f["type"] != "console" {
		t.Fatalf("the healthy subscriber received %v", f)
	}
}
