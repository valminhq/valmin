package ws

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// TestRevokingAGrantDropsThatTopicAndKeepsTheConnection is 14 §6's first row, and the
// reason this file exists: a WebSocket makes no further HTTP requests, so nothing re-checks
// it. An admin who revokes a grant and watches the UI update has every reason to believe
// the access is gone, while the revoked user's console keeps streaming until they close the
// tab.
func TestRevokingAGrantDropsThatTopicAndKeepsTheConnection(t *testing.T) {
	var revoked atomic.Bool
	e := newEnv(t, &Config{
		Authz: canFunc(func(_ *store.User, act authz.Action, instanceID string) bool {
			if revoked.Load() && instanceID == instA && act == authz.ConsoleRead {
				return false
			}
			return true
		}),
		Res: fakeRes{instances: map[string]bool{instA: true, instB: true}},
	})
	e.user = member
	c := e.dial(t, "s1")

	subscribe(t, c, "instance."+instA+".console", "instance."+instA+".stats", "instance."+instB+".console")
	for range 3 {
		if f := read(t, c); f["type"] != "subscribed" {
			t.Fatalf("subscribe failed: %v", f)
		}
	}

	revoked.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); e.hub.GrantChanged(context.Background(), member.ID, instA) }()

	f := read(t, c)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revocation did not reach the socket within a second")
	}

	if f["type"] != "error" || f["topic"] != "instance."+instA+".console" {
		t.Fatalf("the revoked topic was not dropped: %v", f)
	}
	// forbidden here, not not_found — unlike a subscribe, where the caller must not
	// learn the instance exists. This user demonstrably could see it a moment ago, so the
	// oracle is already open and the honest code is the useful one.
	if f["code"] != apierr.Forbidden.String() {
		t.Errorf("code = %v, want forbidden", f["code"])
	}

	// The connection survives. The user may still hold other instances, and closing it
	// would log them out of everything because an admin narrowed one grant.
	//
	// The pong is also what proves the narrowing was per topic: a drop of stats on A or
	// console on B would have queued its own error ahead of it, and this read would have
	// returned that instead.
	send(t, c, map[string]any{"type": "ping"})
	if f := read(t, c); f["type"] != "pong" {
		t.Fatalf("the connection was closed, or another topic was dropped: %v", f)
	}
}

// TestSessionAndUserRevocationClose4403 is 14 §6's session rows. 4403 is *not* 4401: a
// client that conflates them logs the user out because an admin narrowed a grant.
func TestSessionAndUserRevocationClose4403(t *testing.T) {
	t.Run("session", func(t *testing.T) {
		e := newEnv(t, &Config{})
		c := e.dial(t, "s1")
		other := e.dial(t, "s2")
		e.hub.SessionRevoked("s1")

		if code := closeCode(t, c); code != statusAccessRevoked {
			t.Errorf("close code = %v, want 4403", code)
		}
		send(t, other, map[string]any{"type": "ping"})
		if f := read(t, other); f["type"] != "pong" {
			t.Errorf("another session of the same user was closed too: %v", f)
		}
	})

	t.Run("user", func(t *testing.T) {
		e := newEnv(t, &Config{})
		first, second := e.dial(t, "s1"), e.dial(t, "s2")
		e.hub.UserRevoked(admin.ID)

		for i, c := range []*websocket.Conn{first, second} {
			if code := closeCode(t, c); code != statusAccessRevoked {
				t.Errorf("connection %d closed with %v, want 4403", i, code)
			}
		}
	})
}

// TestAbsoluteExpiryClosesWith4401OnASilentSocket is D16. It needs a timer, not a
// check: this connection makes no request after connecting, and an absolute expiry is only
// ever noticed on a request — so without the timer, "sessions expire after N hours" is
// false for exactly the connection that matters most.
func TestAbsoluteExpiryClosesWith4401OnASilentSocket(t *testing.T) {
	e := newEnv(t, &Config{
		SessionExpiry: func(context.Context, string) (time.Time, error) {
			return time.Now().Add(80 * time.Millisecond), nil
		},
	})
	c := e.dial(t, "s1")

	start := time.Now()
	code := closeCode(t, c)
	if code != statusSessionExpired {
		t.Fatalf("close code = %v, want 4401", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the socket closed after %v; nothing was read on it, so only a timer could", elapsed)
	}
}

// TestAnUnreadableSessionExpiryClosesRatherThanRunForever: if the panel cannot tell when a
// session ends, an unbounded socket is the wrong side to fail on.
func TestAnUnreadableSessionExpiryClosesRatherThanRunForever(t *testing.T) {
	e := newEnv(t, &Config{
		SessionExpiry: func(context.Context, string) (time.Time, error) {
			return time.Time{}, context.DeadlineExceeded
		},
	})
	c := e.dial(t, "s1")
	if code := closeCode(t, c); code != statusSessionExpired {
		t.Errorf("close code = %v, want 4401", code)
	}
}

// TestDeletingAnInstanceDropsItsTopicsAndClosesNothing is 14 §6's last row.
func TestDeletingAnInstanceDropsItsTopicsAndClosesNothing(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true, instB: true}}})
	c := e.dial(t, "s1")

	subscribe(t, c, "instance."+instA+".state", "instance."+instB+".state")
	for range 2 {
		read(t, c)
	}

	e.hub.InstanceDeleted(instA)
	f := read(t, c)
	if f["type"] != "error" || f["topic"] != "instance."+instA+".state" {
		t.Fatalf("the deleted instance's topic was not dropped: %v", f)
	}
	send(t, c, map[string]any{"type": "ping"})
	if f := read(t, c); f["type"] != "pong" {
		t.Errorf("the connection was closed by a deletion: %v", f)
	}
}

// TestPublishStateReachesOnlySubscribers: state is pushed by its writers (14 §4.4), so the
// hub has to be the one filtering, and a connection that never asked for an instance must
// not learn it exists.
func TestPublishStateReachesOnlySubscribers(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true, instB: true}}})
	c := e.dial(t, "s1")

	subscribe(t, c, "instance."+instA+".state")
	read(t, c)

	e.hub.PublishState(instB, "running", false)
	e.hub.PublishState(instA, "stopped", true)

	f := read(t, c)
	if f["type"] != "state" || f["instance"] != instA {
		t.Fatalf("received %v; B was not subscribed to", f)
	}
	if f["state"] != "stopped" || f["restart_required"] != true {
		t.Errorf("state message = %v", f)
	}
}
