//go:build integration

// WP-M1-25's other half: the acceptance tests that need a real panel *process*.
//
// AT1 and `12 §9.2`'s crash-recovery scenarios both turn on what happens when the panel
// dies without warning. An in-process test cannot be SIGKILLed, so these build the real
// binary, run it against a real Docker daemon and the stub image, kill it, and restart it.
// D1, D2 and AT2 ask questions about the HTTP surface instead and live in internal/api.
//
// `↯` Every game container here is created directly rather than by a provision job. The
// clone must run as uid 10000 and is never repaired (A4, Q14), so on any host that is not
// uid 10000 — which is every dev machine and the CI runner — a provision cannot reach a
// container at all. Chaining onto one would make every assertion below unreachable exactly
// where a broken recovery path needs to be caught. TestCrashDuringProvision is the one
// test here that does run a real provision, because the interruption *is* its subject.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	stubImage     = "valmin/valheim-stub:dev"
	steamCMDImage = "valmin/steamcmd-stub:dev"
	// leaseTTL is short so a restart after SIGKILL is not blocked for the real 30 s. The
	// sweep identifies a dead process by its owner string, never by expiry (12 §9.1), so
	// shortening this changes nothing the tests are about.
	leaseTTL = 2 * time.Second
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		_ = os.RemoveAll(filepath.Dir(binPath))
	}
	os.Exit(code)
}

// valmind builds the daemon once for the whole package. It is the real binary, not a
// handler under test: AT1's claim is about a process, and a process is what it kills.
func valmind(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "valmind-acceptance-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "valmind")
		if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/valmind: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// logBuffer collects the daemon's stdout and stderr. The setup token is printed to stdout
// (10 §6) and the recovery ordering of 12 §9.1 is asserted against the log, so both have to
// be readable while the process is still writing to them.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// panel is one valmind process against one data root, restartable in place.
type panel struct {
	t      *testing.T
	root   string
	addr   string
	origin string
	env    map[string]string

	out  *logBuffer
	cmd  *exec.Cmd
	wait chan error

	client  *http.Client
	session string
	csrf    string
}

// newPanel prepares a data root and a port but starts nothing: several tests seed a
// container and an instances row *before* the first boot, because reconciliation adopting
// them is the thing under test.
func newPanel(t *testing.T, env map[string]string) *panel {
	t.Helper()
	port := freePort(t)
	p := &panel{
		t: t, root: t.TempDir(), addr: fmt.Sprintf("127.0.0.1:%d", port),
		env: env, out: &logBuffer{},
		client: &http.Client{Timeout: 30 * time.Second},
	}
	p.origin = "http://" + p.addr
	t.Cleanup(p.stop)
	return p
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// start boots the daemon and waits for /healthz.
//
// `↯` It retries while the previous process's daemon lease is still live. A SIGKILLed panel
// releases neither the lease nor the flock, and refusing to start is the correct behaviour
// (C7, ADR-031) — the tests below kill the panel on purpose, so they are the ones that have
// to wait it out rather than the daemon that has to be lenient.
func (p *panel) start() {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		if err := p.launch(); err == nil {
			return
		} else if !strings.Contains(err.Error(), "daemon lease") || time.Now().After(deadline) {
			p.t.Fatalf("start valmind (attempt %d): %v", attempt, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (p *panel) launch() error {
	env := map[string]string{
		"VALMIN_SERVER_LISTEN":       p.addr,
		"VALMIN_SERVER_EXTERNAL_URL": p.origin,
		"VALMIN_DATA_ROOT":           p.root,
		"VALMIN_DATA_HOST_ROOT":      p.root,
		"VALMIN_GAME_IMAGE":          stubImage,
		"VALMIN_GAME_STEAMCMD_IMAGE": steamCMDImage,
		"VALMIN_JOBS_LEASE_TTL":      leaseTTL.String(),
		"VALMIN_JOBS_READY_SETTLE":   "3s",
		"VALMIN_JOBS_READY_TIMEOUT":  "30s",
		"VALMIN_LOG_FORMAT":          "text",
	}
	for k, v := range p.env {
		env[k] = v
	}
	// A fresh environment, not the test's: a stray VALMIN_* in a developer's shell would
	// otherwise silently reconfigure the panel under test.
	cmd := exec.Command(valmind(p.t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = p.out
	cmd.Stderr = p.out
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd, p.wait = cmd, make(chan error, 1)
	// Closed after the result is delivered, so a later receive returns immediately: the
	// health poll below reads this channel to notice an exit, and kill would otherwise
	// block forever waiting for a result something else already took.
	go func(c *exec.Cmd, done chan error) { done <- c.Wait(); close(done) }(cmd, p.wait)

	if err := p.awaitHealthy(); err != nil {
		p.stop()
		return err
	}
	return nil
}

// healthy is one /healthz probe. It is unauthenticated and never touches the database
// (11 §10), which is what makes it usable as the "is the process up" signal here.
func (p *panel) healthy() bool {
	req, err := http.NewRequestWithContext(p.t.Context(), http.MethodGet, p.origin+"/healthz", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *panel) awaitHealthy() error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.wait:
			p.cmd = nil
			return fmt.Errorf("the daemon exited before listening (%w): %s", err, p.out.String())
		default:
		}
		if p.healthy() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the daemon never became healthy: %s", p.out.String())
}

// kill is AT1's instrument and the crash-recovery tests': SIGKILL, no shutdown path, no
// lease release.
func (p *panel) kill() {
	p.t.Helper()
	if p.cmd == nil {
		return
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		p.t.Fatalf("SIGKILL the panel: %v", err)
	}
	<-p.wait
	p.cmd = nil
}

// stop ends the process however it can, for cleanup rather than for a test's assertion.
func (p *panel) stop() {
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.wait
	p.cmd = nil
}

// restart is a kill the panel does not survive followed by the boot that has to clean up
// after it. The log is cleared first so 12 §9.1's ordering can be read off the new boot's
// output alone.
func (p *panel) restart() {
	p.t.Helper()
	p.out.Reset()
	p.start()
}

// setup consumes the printed token and becomes the first admin (10 §6, ADR-023).
func (p *panel) setup() {
	p.t.Helper()
	token := p.printedToken()
	resp := p.do(http.MethodPost, "/api/v1/setup", map[string]string{
		"token": token, "username": "ada", "password": "a-fine-acceptance-password",
	})
	if resp.status != http.StatusOK {
		p.t.Fatalf("setup = %d (%s)", resp.status, resp.body)
	}
	for _, c := range resp.cookies {
		switch c.Name {
		case "valmin_session":
			p.session = c.Value
		case "valmin_csrf":
			p.csrf = c.Value
		}
	}
	if p.session == "" || p.csrf == "" {
		p.t.Fatal("setup set no session or csrf cookie")
	}
}

// printedToken finds the setup token in the framed stdout block. The frame is a long run of
// "=", which is otherwise the same shape as the token, so it is excluded explicitly.
func (p *panel) printedToken() string {
	p.t.Helper()
	for _, line := range strings.Split(p.out.String(), "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 40 && !strings.Contains(line, " ") && strings.Trim(line, "=") != "" {
			return line
		}
	}
	p.t.Fatalf("no setup token printed:\n%s", p.out.String())
	return ""
}

// response is what do hands back. `↯` It is not an *http.Response: the body is read and
// closed inside do, and returning the response as well would leave every caller looking
// like a leak to anything that checks.
type response struct {
	status  int
	body    string
	cookies []*http.Cookie
}

func (p *panel) do(method, path string, body any) response {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			p.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(p.t.Context(), method, p.origin+path, r)
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Origin", p.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.session != "" {
		req.AddCookie(&http.Cookie{Name: "valmin_session", Value: p.session})
		req.AddCookie(&http.Cookie{Name: "valmin_csrf", Value: p.csrf})
		req.Header.Set("X-CSRF-Token", p.csrf)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return response{status: resp.StatusCode, body: string(raw), cookies: resp.Cookies()}
}

// submit posts an operation and returns its job id (11 §3: 202 and a job, never the
// resource).
func (p *panel) submit(path string) string {
	p.t.Helper()
	resp := p.do(http.MethodPost, path, nil)
	if resp.status != http.StatusAccepted {
		p.t.Fatalf("POST %s = %d, want 202 (%s)", path, resp.status, resp.body)
	}
	var stub struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(resp.body), &stub); err != nil {
		p.t.Fatalf("decode job stub %q: %v", resp.body, err)
	}
	return stub.JobID
}

type jobRow struct {
	Status    string  `json:"status"`
	Kind      string  `json:"kind"`
	ErrorCode *string `json:"error_code"`
}

// awaitJob polls at 500 ms, not faster: the chain's per-IP limit is 300 requests a minute
// (11 §7), and a tighter loop spends the whole budget and then decodes a 429 as a job.
func (p *panel) awaitJob(jobID string) jobRow {
	p.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last jobRow
	for time.Now().Before(deadline) {
		resp := p.do(http.MethodGet, "/api/v1/jobs/"+jobID, nil)
		if resp.status != http.StatusOK {
			p.t.Fatalf("GET job %s = %d (%s)", jobID, resp.status, resp.body)
		}
		if err := json.Unmarshal([]byte(resp.body), &last); err != nil {
			p.t.Fatalf("decode job %q: %v", resp.body, err)
		}
		switch last.Status {
		case "succeeded", "failed", "cancelled":
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.t.Fatalf("job %s never reached a terminal status, last = %+v", jobID, last)
	return jobRow{}
}

func (p *panel) state(instanceID string) string {
	p.t.Helper()
	resp := p.do(http.MethodGet, "/api/v1/instances/"+instanceID, nil)
	if resp.status != http.StatusOK {
		p.t.Fatalf("GET instance %s = %d (%s)", instanceID, resp.status, resp.body)
	}
	var inst struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resp.body), &inst); err != nil {
		p.t.Fatalf("decode instance %q: %v", resp.body, err)
	}
	return inst.State
}

// awaitState catches a transient state before the panel is killed, so it polls faster than
// awaitJob — but at 200 ms, not as fast as it could. `↯` The chain's per-IP limiter is 300
// requests a minute (11 §7): a 10 ms loop spends the whole budget in half a minute and then
// decodes a 429 as an instance, which is a failure that reads like a state machine bug.
func (p *panel) awaitState(instanceID string, want ...string) {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = p.state(instanceID)
		if slices.Contains(want, got) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.t.Fatalf("%s is %q; it never reached any of %v", instanceID, got, want)
}

// docker is the test's own client, so it can ask Docker what happened while the panel is
// dead — which is the whole of AT1.
func docker(t *testing.T) *runtime.Docker {
	t.Helper()
	d, err := runtime.NewDocker(t.Context(), "unix:///var/run/docker.sock", "")
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// seedInstance creates a real stub container and the instances row pointing at it, before
// the panel has ever run. The labels are the real ones: reconciliation joins Docker to the
// database on io.valmin.instance.id (08 §6.1), so a container seeded without them would
// make the adoption pass for the wrong reason.
//
// `↯` The instance id carries a random suffix. These tests kill the panel on purpose, so an
// aborted run leaves a labelled container behind — and the next run's reconciliation would
// join on that label and adopt the corpse, which reads as the panel getting the answer
// wrong rather than as the run before it not having tidied up.
func seedInstance(t *testing.T, p *panel, d *runtime.Docker, base string, env ...string) (name, containerID string) {
	t.Helper()
	name = base + "-" + suffix()
	containerID, err := d.Create(t.Context(), &runtime.ContainerSpec{
		Name:       instance.ContainerName(name) + "-" + suffix(),
		Image:      stubImage,
		Env:        env,
		Labels:     instance.Labels(name, 2456),
		StopSignal: "SIGINT",
		// The real floor (ADR-008). It matters here: after the panel is killed mid-stop,
		// dockerd is the one still holding the escalation timer.
		StopTimeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), containerID, true) })

	dataDir := filepath.Join(p.root, "instances", name)
	if err := os.MkdirAll(filepath.Join(dataDir, "worlds"), 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(p.root, "panel.db"))
	if err != nil {
		t.Fatalf("open the panel database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Writer.ExecContext(t.Context(), `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, 2456, 'Server', 'World', 'v1.k.n.ct', ?, ?, ?)`,
		name, name, containerID, dataDir, "cp-"+name, store.Now(), store.Now()); err != nil {
		t.Fatalf("seed instance row: %v", err)
	}
	return name, containerID
}

// suffix is a per-container name discriminator. `↯` It is the *tail* of the id, not the
// head: store.NewID is a UUIDv7, whose leading hex is the timestamp, so two containers
// created in the same minute — or one run and the next — collide on a prefix and Docker
// refuses the name. The panel itself never resolves a container by name (08 §1), but a
// test that cannot create one is stuck all the same.
func suffix() string {
	id := store.NewID()
	return id[len(id)-6:]
}

// awaitContainerRunning waits on Docker rather than on the panel, so it costs the rate
// limiter nothing and reports what the job actually did rather than what the row says.
func awaitContainerRunning(t *testing.T, d *runtime.Docker, containerID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if inspect(t, d, containerID).Running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("container %s never started", containerID)
}

func inspect(t *testing.T, d *runtime.Docker, containerID string) runtime.Container {
	t.Helper()
	c, err := d.Inspect(t.Context(), containerID)
	if err != nil {
		t.Fatalf("inspect %s: %v", containerID, err)
	}
	return c
}

// TestAT1KillingThePanelLeavesTheServerRunning is `05` M1's first acceptance test and
// C10 / G6 stated as an experiment: the panel is not load-bearing. Killing it must not
// touch a game container, and players stay connected because nothing signalled the process
// they are connected to.
func TestAT1KillingThePanelLeavesTheServerRunning(t *testing.T) {
	p := newPanel(t, nil)
	d := docker(t)
	id, containerID := seedInstance(t, p, d, "at1")

	p.start()
	p.setup()

	if got := p.state(id); got != "stopped" {
		t.Fatalf("state = %q after reconciliation, want stopped", got)
	}
	if job := p.awaitJob(p.submit("/api/v1/instances/" + id + "/start")); job.Status != "succeeded" {
		t.Fatalf("start = %+v, want succeeded", job)
	}

	before := inspect(t, d, containerID)
	if !before.Running {
		t.Fatal("the game container is not running after a succeeded start")
	}

	p.kill()

	// `↯` The assertion AT1 exists for. Not "a container with this id is running" — the
	// *same* run, unrestarted: a panel that took the server down and `unless-stopped` put
	// it back would leave a running container and a new start time, and players would have
	// been disconnected for the gap.
	after := inspect(t, d, containerID)
	if !after.Running {
		t.Error("the game container stopped when the panel was killed (C10, G6, 01 §6)")
	}
	if !after.StartedAt.Equal(before.StartedAt) {
		t.Errorf("the game container restarted across the panel's death: started at %s, now %s",
			before.StartedAt, after.StartedAt)
	}
	if after.RestartCount != before.RestartCount {
		t.Errorf("restart count moved from %d to %d", before.RestartCount, after.RestartCount)
	}
	if p.healthy() {
		t.Fatal("the panel answered /healthz after SIGKILL; it was not actually dead")
	}

	p.restart()
	if got := p.state(id); got != "running" {
		t.Errorf("state = %q after the panel came back, want running — reconciliation did "+
			"not find the container it never stopped (08 §6.1)", got)
	}
	if again := inspect(t, d, containerID); !again.StartedAt.Equal(before.StartedAt) {
		t.Errorf("the restarted panel restarted the container: started at %s, now %s",
			before.StartedAt, again.StartedAt)
	}
}

// TestCrashDuringStartResolvesOffTheLog is 12 §9.2's `starting` + running row end to end.
// The stub never announces readiness, so the recovered instance must still land in
// `running` with the registration unconfirmed (ADR-043, E6) rather than in `error`.
func TestCrashDuringStartResolvesOffTheLog(t *testing.T) {
	// A long settle keeps the start job inside its readiness window while the panel is
	// killed, which is what makes "mid-start" a state rather than a race.
	p := newPanel(t, map[string]string{"VALMIN_JOBS_READY_SETTLE": "25s"})
	d := docker(t)
	id, containerID := seedInstance(t, p, d, "crash-start", "STUB_MODE=no-ready")

	p.start()
	p.setup()
	p.submit("/api/v1/instances/" + id + "/start")
	p.awaitState(id, "starting")
	// `↯` The row reaches `starting` in the claim transaction, *before* Docker is asked to
	// start anything (12 §6). Killing on the row alone lands in the gap where there is no
	// container to outlive the panel, which is a different recovery row entirely — so wait
	// for the container, then confirm the job is still inside its readiness window.
	awaitContainerRunning(t, d, containerID)
	if got := p.state(id); got != "starting" {
		t.Fatalf("state = %q; the start finished before the panel could be killed", got)
	}
	p.kill()

	if !inspect(t, d, containerID).Running {
		t.Fatal("the container is not running: the crash did not land mid-start")
	}

	p.restart()
	if got := p.state(id); got != "running" {
		t.Errorf("state = %q, want running: the container outlived the process that started "+
			"it, and a missing readiness line is a warning, not a failure (ADR-043)", got)
	}
}

// TestCrashDuringStopParksInError is 12 §9.2's `stopping` + running row. The stub sits in
// its save path long past the panel's death, so the next boot meets a stop that was
// requested and demonstrably did not complete — which is `error`, not `stopped`.
func TestCrashDuringStopParksInError(t *testing.T) {
	p := newPanel(t, nil)
	d := docker(t)
	// The delay is inside the SIGINT trap, so the container is still up when the panel
	// comes back. dockerd holds the 120 s escalation timer, not the panel (ADR-008).
	id, containerID := seedInstance(t, p, d, "crash-stop", "STUB_SAVE_DELAY=600")

	p.start()
	p.setup()
	if job := p.awaitJob(p.submit("/api/v1/instances/" + id + "/start")); job.Status != "succeeded" {
		t.Fatalf("start = %+v, want succeeded", job)
	}
	p.submit("/api/v1/instances/" + id + "/stop")
	p.awaitState(id, "stopping")
	p.kill()

	if !inspect(t, d, containerID).Running {
		t.Fatal("the container already exited: the crash did not land mid-stop")
	}

	p.restart()
	if got := p.state(id); got != "error" {
		t.Errorf("state = %q, want error: a stop was requested and the server is still up", got)
	}
}

// TestCrashDuringProvisionSweepsBeforeItReconciles is C6, and it is asserted the way the
// plan asks — by the order the daemon's own log reports, not by inspecting a function.
// Reconciling first means meeting an instance in a transient state whose lock is held by a
// process that no longer exists.
func TestCrashDuringProvisionSweepsBeforeItReconciles(t *testing.T) {
	p := newPanel(t, nil)
	p.start()
	p.setup()

	resp := p.do(http.MethodPost, "/api/v1/instances", map[string]any{
		"name": "crash-provision-" + suffix(), "server_name": "My Server",
		"world_name": "MyWorld", "password": "hunter2",
	})
	if resp.status != http.StatusAccepted {
		t.Fatalf("create = %d, want 202 (%s)", resp.status, resp.body)
	}
	var stub struct {
		JobID      string `json:"job_id"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(resp.body), &stub); err != nil {
		t.Fatal(err)
	}

	// The SteamCMD phase runs a real throwaway container, so `provisioning` is a window of
	// seconds, not an instant. Landing outside it is a broken premise, not a flaky test,
	// and awaitState says so rather than passing vacuously.
	p.awaitState(stub.InstanceID, "provisioning")
	p.kill()

	p.restart()

	log := p.out.String()
	swept := strings.Index(log, "swept dead job")
	if swept < 0 {
		t.Fatalf("the boot after a crash swept no dead job:\n%s", log)
	}
	reconciled := -1
	for _, marker := range []string{"re-submitted an interrupted job", "reconciled instance"} {
		if at := strings.Index(log, marker); at >= 0 && (reconciled < 0 || at < reconciled) {
			reconciled = at
		}
	}
	if reconciled < 0 {
		t.Fatalf("the boot after a crash reconciled nothing:\n%s", log)
	}
	if swept > reconciled {
		t.Errorf("reconciliation ran before the job sweep (C6, 12 §9.1 step 2 before step 3):\n%s", log)
	}

	// The interrupted job is closed out rather than left `running` forever, and the
	// instance leaves the transient state it was killed in (12 §9.2).
	if job := p.awaitJob(stub.JobID); job.Status != "failed" ||
		job.ErrorCode == nil || *job.ErrorCode != "interrupted" {
		t.Errorf("the swept job = %+v, want failed/interrupted", job)
	}
	// `↯` And it leaves `provisioning`. Which resting state it lands in depends on the uid:
	// the resumed provision succeeds at 10000 and fails A4's ownership check anywhere else
	// (Q14). Both are answers; staying transient forever is not, and awaitState says so.
	p.awaitState(stub.InstanceID, "error", "stopped")
}
