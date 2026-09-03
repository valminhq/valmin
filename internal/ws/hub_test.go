package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

const (
	instA = "01920000-0000-7000-8000-0000000000aa"
	instB = "01920000-0000-7000-8000-0000000000bb"
	jobA  = "01920000-0000-7000-8000-0000000000c1"
)

// member holds a grant on instance A and nothing on B.
var member = &store.User{ID: "u-member", Username: "mem", Role: store.RoleMember}

func grantOnA(_ *store.User, _ authz.Action, instanceID string) bool { return instanceID == instA }

// TestMemberCannotSeeAnotherInstanceOverTheSocket covers instance-scoped isolation on the
// socket. The frame is built by hand rather than through any UI, because a check the UI
// enforces by never offering the topic is not a check.
func TestMemberCannotSeeAnotherInstanceOverTheSocket(t *testing.T) {
	e := newEnv(t, &Config{
		Authz: canFunc(grantOnA),
		Res:   fakeRes{instances: map[string]bool{instA: true, instB: true}},
	})
	e.user = member
	c := e.dial(t, "s1")

	subscribe(t, c, "instance."+instA+".console", "instance."+instB+".console")

	first, second := read(t, c), read(t, c)
	if first["type"] != "subscribed" || first["topic"] != "instance."+instA+".console" {
		t.Fatalf("A was not acknowledged: %v", first)
	}
	if second["type"] != "error" {
		t.Fatalf("B was not refused: %v", second)
	}
	// not_found, never forbidden (D2, 14 §2.3). forbidden would confirm that B exists,
	// which is the enumeration oracle 11 §2.3 closes in REST and this closes here.
	if second["code"] != apierr.NotFound.String() {
		t.Errorf("B was refused with %q, want %q", second["code"], apierr.NotFound.String())
	}
	if second["topic"] != "instance."+instB+".console" {
		t.Errorf("the error names no topic: %v", second)
	}
}

// TestOneBadTopicDoesNotFailTheOthers is 14 §2.3: acknowledgement is per topic.
func TestOneBadTopicDoesNotFailTheOthers(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true}}})
	c := e.dial(t, "s1")

	subscribe(t, c,
		"instance."+instA+".console", "instance.*.console",
		"instance."+instA+".stats", "instance."+instB+".state",
		"instance."+instA+".state")

	got := map[string]string{}
	for range 5 {
		f := read(t, c)
		got[f["topic"].(string)] = f["type"].(string)
	}
	for _, topic := range []string{
		"instance." + instA + ".console", "instance." + instA + ".stats", "instance." + instA + ".state",
	} {
		if got[topic] != "subscribed" {
			t.Errorf("%s = %q, want subscribed", topic, got[topic])
		}
	}
	if got["instance.*.console"] != "error" {
		t.Errorf("a wildcard was accepted")
	}
	if got["instance."+instB+".state"] != "error" {
		t.Errorf("an unknown instance was accepted")
	}
}

// TestAnAdminCannotSubscribeToAnInstanceThatDoesNotExist: Can says yes to an admin for
// every instance id, so existence has to be asked separately or a typo becomes a
// subscription that waits forever for a message with no source.
func TestAnAdminCannotSubscribeToAnInstanceThatDoesNotExist(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true}}})
	c := e.dial(t, "s1")

	subscribe(t, c, "instance."+instB+".console")
	if f := read(t, c); f["type"] != "error" || f["code"] != apierr.NotFound.String() {
		t.Errorf("an admin subscribed to a nonexistent instance: %v", f)
	}
}

// TestJobTopicAuthorizesAgainstTheJobsInstance is 09 §4.1's subtle one: the topic string
// carries no instance, so the row is resolved first.
func TestJobTopicAuthorizesAgainstTheJobsInstance(t *testing.T) {
	const jobOnB = "01920000-0000-7000-8000-0000000000c2"
	e := newEnv(t, &Config{
		Authz: canFunc(grantOnA),
		Res: fakeRes{
			instances: map[string]bool{instA: true, instB: true},
			jobs:      map[string]string{jobA: instA, jobOnB: instB},
		},
	})
	e.user = member
	c := e.dial(t, "s1")

	subscribe(t, c, "job."+jobA)
	if f := read(t, c); f["type"] != "subscribed" {
		t.Fatalf("a job on the granted instance was refused: %v", f)
	}
	subscribe(t, c, "job."+jobOnB)
	if f := read(t, c); f["type"] != "error" || f["code"] != apierr.NotFound.String() {
		t.Errorf("a job on an ungranted instance was allowed: %v", f)
	}
	subscribe(t, c, "job.01920000-0000-7000-8000-0000000000cf")
	if f := read(t, c); f["type"] != "error" {
		t.Errorf("an unknown job was allowed: %v", f)
	}
}

// TestAGlobalJobIsAdminOnly covers both cases 14 §2.2 folds together: a genuinely global
// kind, and a job whose instance was deleted — 12 §4.2 sets the column NULL rather than
// cascading the history away (C15), so they arrive here indistinguishable, and both are
// admin-only.
func TestAGlobalJobIsAdminOnly(t *testing.T) {
	res := fakeRes{jobs: map[string]string{jobA: ""}}

	e := newEnv(t, &Config{Authz: canFunc(grantOnA), Res: res})
	e.user = member
	c := e.dial(t, "s1")
	subscribe(t, c, "job."+jobA)
	if f := read(t, c); f["type"] != "error" {
		t.Errorf("a member subscribed to a global job: %v", f)
	}

	e2 := newEnv(t, &Config{Res: res})
	c2 := e2.dial(t, "s1")
	subscribe(t, c2, "job."+jobA)
	if f := read(t, c2); f["type"] != "subscribed" {
		t.Errorf("an admin was refused a global job: %v", f)
	}
}

// TestATerminalJobIsStillSubscribable: the client that subscribes a moment after the job
// succeeded gets the topic and reads the outcome from GET /jobs/{id}. Refusing would make
// 12 §7's subscribe-then-fetch ordering unimplementable — which is the ordering that stops
// a job completing in the gap between the two calls from being lost.
func TestATerminalJobIsStillSubscribable(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true}, jobs: map[string]string{jobA: instA}}})
	c := e.dial(t, "s1")
	subscribe(t, c, "job."+jobA)
	if f := read(t, c); f["type"] != "subscribed" {
		t.Errorf("a finished job could not be subscribed to: %v", f)
	}
}

// TestUpgradeWithoutOriginIsRejectedInTheEnvelope is 11 §6.3, and it is the opposite of the
// REST rule on purpose: browsers always send Origin on an upgrade, so a missing one is not
// a curl user to accommodate — it is cross-site WebSocket hijacking, which reads every
// console stream the victim can see.
func TestUpgradeWithoutOriginIsRejectedInTheEnvelope(t *testing.T) {
	e := newEnv(t, &Config{})

	for _, tc := range []struct{ name, origin string }{
		{"absent", ""},
		{"another site", "https://evil.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.dialRaw(t, "?session=s1&csrf=csrf-s1", tc.origin)
			var failure *dialFailure
			if !asDialFailure(err, &failure) {
				t.Fatalf("the upgrade was not rejected: %v", err)
			}
			if failure.resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", failure.resp.StatusCode)
			}
			// A rejected client must not have to parse a close frame to learn why, so
			// the answer is the ordinary envelope, before the upgrade.
			if ct := failure.resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body struct {
				Error struct{ Code string } `json:"error"`
			}
			if err := json.NewDecoder(failure.resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != apierr.OriginRejected.String() {
				t.Errorf("code = %q, want %q", body.Error.Code, apierr.OriginRejected.String())
			}
		})
	}
}

func asDialFailure(err error, out **dialFailure) bool {
	return errors.As(err, out)
}

func TestUpgradeRequiresASessionAndACSRFToken(t *testing.T) {
	e := newEnv(t, &Config{})

	if _, err := e.dialRaw(t, "?csrf=csrf-s1", testOrigin); err == nil {
		t.Error("an unauthenticated upgrade succeeded")
	}
	if _, err := e.dialRaw(t, "?session=s1&csrf=wrong", testOrigin); err == nil {
		t.Error("a wrong csrf token was accepted")
	}
}

// TestTheCSRFTokenMayArriveAsTheFirstMessage is 11 §6.3's other half: a browser cannot set
// a header on the WebSocket constructor, so the token comes as a query parameter or the
// first frame. A first frame that is not it closes the connection.
func TestTheCSRFTokenMayArriveAsTheFirstMessage(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true}}})

	good, err := e.dialRaw(t, "?session=s1", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	send(t, good, map[string]any{
		"type": "subscribe", "csrf": "csrf-s1", "topics": []string{"instance." + instA + ".state"},
	})
	if f := read(t, good); f["type"] != "subscribed" {
		t.Errorf("a first-message token was not accepted: %v", f)
	}

	bad, err := e.dialRaw(t, "?session=s2", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	subscribe(t, bad, "instance."+instA+".state")
	if code := closeCode(t, bad); code != websocket.StatusPolicyViolation {
		t.Errorf("close code = %v, want 1008", code)
	}
}

// TestConsoleCommandIsUnsupported is 14 §7.3 and E3. The hub does not execute commands; on
// this build the channel resolves to none, and 03 §7 measured zero reads on fd 0.
func TestConsoleCommandIsUnsupported(t *testing.T) {
	e := newEnv(t, &Config{Res: fakeRes{instances: map[string]bool{instA: true}}})
	c := e.dial(t, "s1")

	send(t, c, map[string]any{"type": "console.command", "instance": instA, "command": "save"})
	f := read(t, c)
	if f["type"] != "error" || f["code"] != apierr.Unsupported.String() {
		t.Errorf("console.command answered %v, want unsupported", f)
	}
}

func TestApplicationPingIsAnswered(t *testing.T) {
	e := newEnv(t, &Config{})
	c := e.dial(t, "s1")
	send(t, c, map[string]any{"type": "ping"})
	if f := read(t, c); f["type"] != "pong" {
		t.Errorf("ping answered %v", f)
	}
}

// TestTheNinthConnectionClosesTheOldest is 14 §3.3: tabs, a phone, and a stale one.
func TestTheNinthConnectionClosesTheOldest(t *testing.T) {
	e := newEnv(t, &Config{})

	first := e.dial(t, "s1")
	for i := 1; i < maxConnectionsPerUser; i++ {
		// Distinct sessions, one user: the cap is per account, not per session.
		e.dial(t, "s"+string(rune('a'+i)))
		time.Sleep(2 * time.Millisecond)
	}
	// The ninth goes in without waiting to be registered: registering it is what evicts the
	// first, and waiting for the count to reach nine would race that eviction.
	ninth, err := e.dialRaw(t, "?session=s9&csrf=csrf-s9", testOrigin)
	if err != nil {
		t.Fatal(err)
	}

	if code := closeCode(t, first); code != websocket.StatusPolicyViolation {
		t.Errorf("the oldest connection closed with %v, want 1008", code)
	}
	send(t, ninth, map[string]any{"type": "ping"})
	if f := read(t, ninth); f["type"] != "pong" {
		t.Errorf("the newest connection was closed instead: %v", f)
	}
}

func TestTopicsPerConnectionAreCappedWithoutClosing(t *testing.T) {
	instances := map[string]bool{}
	topics := make([]string, 0, maxTopics+1)
	for i := range maxTopics + 1 {
		id := fmt.Sprintf("01920000-0000-7000-8000-%012x", i)
		instances[id] = true
		topics = append(topics, "instance."+id+".state")
	}

	e := newEnv(t, &Config{Res: fakeRes{instances: instances}})
	c := e.dial(t, "s1")
	subscribe(t, c, topics...)

	last := frame{}
	for range len(topics) {
		last = read(t, c)
	}
	// A count limit is an error message, not a close (14 §3.3) — the sixty-four topics
	// that did fit are still working.
	if last["type"] != "error" {
		t.Fatalf("the 65th topic was accepted: %v", last)
	}
	send(t, c, map[string]any{"type": "ping"})
	if f := read(t, c); f["type"] != "pong" {
		t.Errorf("the connection was closed over a count limit: %v", f)
	}
}

// TestShutdownClosesWith1001 is 11 §10: the SPA reconnects quietly rather than showing an
// error, and the daemon does not wait out its whole grace period for a handler that only
// returns when its socket closes.
func TestShutdownClosesWith1001(t *testing.T) {
	e := newEnv(t, &Config{})
	c := e.dial(t, "s1")
	e.hub.Close()
	if code := closeCode(t, c); code != websocket.StatusGoingAway {
		t.Errorf("close code = %v, want 1001", code)
	}
}

func TestMalformedJSONIsAnErrorNotAClose(t *testing.T) {
	e := newEnv(t, &Config{})
	c := e.dial(t, "s1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if f := read(t, c); f["code"] != apierr.MalformedJSON.String() {
		t.Errorf("malformed json answered %v", f)
	}
}
