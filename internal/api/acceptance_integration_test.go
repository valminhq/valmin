//go:build integration

// The panel's end-to-end acceptance tests, the ones that are easiest to skip.
//
// D1, D2 and AT2 live here because they are questions about the panel's own surface and
// can be asked in-process against a real Docker daemon and the stub image. AT1 and the
// crash-recovery scenarios of 12 §9.2 cannot: they need a panel process that can actually
// be SIGKILLed, and they live in cmd/valmind.
//
// D1 is driven at the API rather than through a browser. This repo has no browser driver,
// so driving it through one means adding a driver and pinning the markup, which turns
// every later restyle into a test migration. Driving the API exercises the same endpoints
// the SPA does, which is what the rest of this package already assumes: AT2 builds its
// subscribe frame by hand precisely so it cannot pass by the UI never offering the topic.
// The SPA's own invariants stay guarded by web/src/lib/ui-invariants.test.ts, a weaker
// instrument than a click-through and a much cheaper one.
//
// The join leg is not here and cannot be. A real Valheim client joining a real server is
// a manual step, recorded with its date and build in docs/. Nothing below covers it.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// nameSuffix is a per-container name discriminator. The *tail* of the id, not the head:
// store.NewID is a UUIDv7 whose leading hex is the timestamp, so two containers created in
// the same minute collide on a prefix and Docker refuses the name.
func nameSuffix() string {
	id := store.NewID()
	return id[len(id)-6:]
}

// acceptanceContainer creates a real stub container carrying the io.valmin.* labels for
// instanceID. publish asks for the two UDP host bindings 08 §5 fixes — which is what makes
// D2 a statement about the host rather than about the instances table.
func acceptanceContainer(
	t *testing.T, d *runtime.Docker, instanceID string, basePort int, publish bool, env ...string,
) string {
	t.Helper()
	spec := &runtime.ContainerSpec{
		User:  testContainerUser,
		Name:  instance.ContainerName(instanceID) + "-" + nameSuffix(),
		Image: integrationGameImage, Env: env,
		Labels:     instance.Labels(instanceID, basePort),
		StopSignal: "SIGINT", StopTimeout: 15 * time.Second,
	}
	if publish {
		spec.Ports = []runtime.Port{
			{HostPort: basePort, ContainerPort: basePort, Proto: "udp"},
			{HostPort: basePort + 1, ContainerPort: basePort + 1, Proto: "udp"},
		}
	}
	id, err := d.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("create container for %s on %d: %v", instanceID, basePort, err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), id, true) })
	return id
}

// seedInstanceOnPort is seedRealInstance with the base port as a parameter, because
// base_port is UNIQUE (A6) and both D2 and AT2 need two instances at once.
func seedInstanceOnPort(
	t *testing.T, rt *Router, db *store.DB, d *runtime.Docker, name string, basePort int, publish bool,
) string {
	t.Helper()
	containerID := acceptanceContainer(t, d, name, basePort, publish)
	dataDir := rt.Supervisor().inst.Cfg.Data.HostRoot + "/instances/" + name
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, ?, 'Server', 'World', 'v1.k.n.ct', ?, ?, ?)`,
		name, name, containerID, dataDir, basePort, "cp-"+name, store.Now(), store.Now())
	return name
}

// setPassword gives a seeded user a hash a real login can verify. fastenArgon2 must already
// have run, or this pays Decision 4's real ~64 MiB cost twice.
func setPassword(t *testing.T, db *store.DB, username, password string) {
	t.Helper()
	params, err := auth.LoadArgon2Params(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(password, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserPasswordByUsername(t.Context(), username, hash); err != nil {
		t.Fatal(err)
	}
}

// TestD1CreateStartStopDelete covers create, start, stop and delete against a real
// daemon, minus the manual join leg.
//
// The provision leg's success is only reachable at uid 10000 (A4, Q14), which is
// production and is not a dev host or a CI runner. Off that uid the ownership check must
// fail the job loudly — a defensive chown would mask a clone that ran as the wrong user —
// so this asserts whichever outcome the environment actually supports, and then creates the
// container provisioning would have created so that start, stop and delete are still proven
// against a real daemon. Provisioning's own end-to-end coverage is
// provision_integration_test.go's.
func TestD1CreateStartStopDelete(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("d1"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if stub.InstanceID == nil {
		t.Fatal("the create job stub carries no instance_id")
	}
	id := *stub.InstanceID
	provision := waitForJobTerminal(t, rt, admin, stub.JobID)

	if os.Getuid() == instance.WantCloneUID {
		if provision.Status != "succeeded" {
			t.Fatalf("provision = %+v, want succeeded at uid %d", provision, instance.WantCloneUID)
		}
	} else {
		if provision.Status != "failed" {
			t.Fatalf("provision = %+v, want failed: this process is uid %d, not %d, and A4's "+
				"ownership check must catch that rather than repair it",
				provision, os.Getuid(), instance.WantCloneUID)
		}
		var basePort int
		if err := db.Reader.QueryRowContext(t.Context(),
			`SELECT base_port FROM instances WHERE id = ?`, id).Scan(&basePort); err != nil {
			t.Fatal(err)
		}
		containerID := acceptanceContainer(t, d, id, basePort, false)
		seed(t, db, `UPDATE instances SET state = 'stopped', container_id = ? WHERE id = ?`,
			containerID, id)
	}

	var dataDir string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT data_dir FROM instances WHERE id = ?`, id).Scan(&dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataDir + "/worlds"); err != nil {
		t.Fatalf("provisioning did not create worlds/: %v", err)
	}

	if got := instanceState(t, rt, admin, id); got != "stopped" {
		t.Fatalf("state = %q before start, want stopped", got)
	}

	if start := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/start"); start.Status != "succeeded" {
		t.Fatalf("start = %+v, want succeeded", start)
	}
	if got := instanceState(t, rt, admin, id); got != "running" {
		t.Fatalf("state = %q after start, want running", got)
	}

	// The join leg belongs here and is manual; it is recorded in docs/.

	stop := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/stop")
	if stop.Status != "succeeded" {
		t.Fatalf("stop = %+v, want succeeded", stop)
	}
	if stop.Clean == nil || !*stop.Clean {
		t.Errorf("clean = %v, want true: the stub reaches the full save literal (B2)", stop.Clean)
	}
	if got := instanceState(t, rt, admin, id); got != "stopped" {
		t.Fatalf("state = %q after stop, want stopped", got)
	}

	if del := runJob(t, rt, admin, http.MethodDelete, "/api/v1/instances/"+id); del.Status != "succeeded" {
		t.Fatalf("delete = %+v, want succeeded", del)
	}
	if rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/"+id, http.NoBody)); rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", rec.Code)
	}
	// 12 §10: keep_worlds defaults to true, and the panel never removes worlds/ outside
	// a delete that asked for it. This is the last line of D1 and the most expensive one to
	// get wrong.
	if _, err := os.Stat(dataDir + "/worlds"); err != nil {
		t.Errorf("worlds/ did not survive a default delete: %v", err)
	}
}

// unusedPorts is an allocator source that reports no reservation at all, so a question can
// be put to the *host* half of the check without the instances table answering it first.
type unusedPorts struct{}

func (unusedPorts) UsedBasePorts(context.Context) (map[int]bool, error) { return nil, nil }

// TestD2TwoInstancesRunConcurrently asserts two servers run at once:
// on a stride-5 allocation, with the conflict check covering both address families (A6).
//
// The last assertion is what a unit test cannot make. ports_test.go proves the
// allocator skips a port a test process is holding; this proves it skips one *Docker* is
// holding, which is the case that actually occurs — and it asks with the database blind, so
// the answer comes from the host probe rather than from base_port UNIQUE.
func TestD2TwoInstancesRunConcurrently(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	cfg := rt.Supervisor().inst.Cfg
	alloc := instance.NewAllocator(db, cfg.Ports.Base, cfg.Ports.Stride)

	portA, err := alloc.Allocate(t.Context())
	if err != nil {
		t.Fatalf("allocate for A: %v", err)
	}
	idA := seedInstanceOnPort(t, rt, db, d, "d2-a", portA, true)
	if start := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+idA+"/start"); start.Status != "succeeded" {
		t.Fatalf("start A = %+v, want succeeded", start)
	}

	portB, err := alloc.Allocate(t.Context())
	if err != nil {
		t.Fatalf("allocate for B: %v", err)
	}
	if portB != portA+cfg.Ports.Stride {
		t.Errorf("B allocated %d, want %d — stride 5 (03 §2)", portB, portA+cfg.Ports.Stride)
	}
	idB := seedInstanceOnPort(t, rt, db, d, "d2-b", portB, true)
	if start := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+idB+"/start"); start.Status != "succeeded" {
		t.Fatalf("start B = %+v, want succeeded — a port collision presents exactly here", start)
	}

	for _, id := range []string{idA, idB} {
		if got := instanceState(t, rt, admin, id); got != "running" {
			t.Errorf("%s is %q, want both running at the same moment", id, got)
		}
	}

	blind := instance.NewAllocator(unusedPorts{}, cfg.Ports.Base, cfg.Ports.Stride)
	got, err := blind.Allocate(t.Context())
	if err != nil {
		t.Fatalf("allocate with the database blind: %v", err)
	}
	if got == portA || got == portB {
		t.Errorf("allocated %d while Docker publishes it — the host check missed a live "+
			"binding (A6: Docker publishes on 0.0.0.0 *and* [::])", got)
	}
}

// TestAT2OperatorOnAIsBlindToB asserts cross-instance blindness. A member holding
// `operator` on A can start A, cannot see B over REST, and cannot see B over the WebSocket
// either — where the answer must be `not_found`, not `forbidden` (D2, ADR-038).
//
// The subscribe frame is built by hand, as a literal topic string. Going through
// ws.ConsoleTopic — or through the SPA — would let this pass because the client never
// offered the topic, which is not the property under test. The hub is the transport that is
// easier to script against, so an enumeration oracle left open here is worth more to an
// attacker than the same oracle in REST.
func TestAT2OperatorOnAIsBlindToB(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	fastenArgon2(t, db)

	idA := seedInstanceOnPort(t, rt, db, d, "at2-a", 2456, false)
	idB := seedInstanceOnPort(t, rt, db, d, "at2-b", 2461, false)

	const password = "a-fine-password-for-opal"
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-member', 'opal', 'argon2id$stub', 'member', ?)`, store.Now())
	setPassword(t, db, "opal", password)
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-member', ?, 'operator', '[]', ?)`, idA, store.Now())

	member := loginAs(t, rt, "opal", password)

	rec := send(rt, authenticated(httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+idA+"/start", http.NoBody), member))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("member starting A = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitForJobTerminal(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("the member's start of A = %+v, want succeeded", final)
	}

	blind := send(rt, authenticated(httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/"+idB, http.NoBody), member))
	if blind.Code != http.StatusNotFound {
		t.Fatalf("member GET B = %d, want 404 (%s)", blind.Code, blind.Body)
	}
	if code := errCode(t, blind); code != "not_found" {
		t.Errorf("code = %q, want not_found — 403 is an existence oracle (D2)", code)
	}

	list := send(rt, authenticated(httptest.NewRequest(
		http.MethodGet, "/api/v1/instances", http.NoBody), member))
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodeInto(t, list, &page)
	if len(page.Items) != 1 || page.Items[0].ID != idA {
		t.Errorf("the member's list = %+v, want only %s", page.Items, idA)
	}

	assertConsoleTopics(t, rt, member, idA, idB)
}

// assertConsoleTopics opens a real socket as the member and subscribes to both consoles in
// one frame — 14 §2.3's per-topic acknowledgement, so the refusal of B must not take A's
// subscription down with it, and the acceptance of A is the control that proves the refusal
// is about authorization rather than about a frame that never parsed.
func assertConsoleTopics(t *testing.T, rt *Router, member *httptest.ResponseRecorder, idA, idB string) {
	t.Helper()
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)

	session := cookieValue(member, "valmin_session")
	csrf := cookieValue(member, "valmin_csrf")
	if session == "" || csrf == "" {
		t.Fatal("the member's login set no session or csrf cookie")
	}

	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", "valmin_session="+session+"; valmin_csrf="+csrf)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws?csrf=" + csrf
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("dial as the member: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	// Hand-built, with the topic spelled out: nothing in this frame comes from the client
	// the SPA ships or from the constructor the hub itself uses.
	frame, err := json.Marshal(map[string]any{
		"type":   "subscribe",
		"topics": []string{"instance." + idA + ".console", "instance." + idB + ".console"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}

	var sawA, sawB bool
	for range 50 {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m struct {
			Type  string `json:"type"`
			Topic string `json:"topic"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("decode %q: %v", data, err)
		}
		switch {
		case m.Type == "subscribed" && m.Topic == "instance."+idA+".console":
			sawA = true
		case m.Type == "error" && m.Topic == "instance."+idB+".console":
			sawB = true
			if m.Code != "not_found" {
				t.Errorf("B's topic = %q, want not_found: forbidden over the socket is the "+
					"same enumeration oracle on an easier transport (D2, 14 §2.3)", m.Code)
			}
		case m.Type == "subscribed" && m.Topic == "instance."+idB+".console":
			t.Fatal("the member was subscribed to an instance they cannot see")
		}
		if sawA && sawB {
			return
		}
	}
	t.Errorf("no per-topic answer for both consoles: A acknowledged = %v, B refused = %v", sawA, sawB)
}
