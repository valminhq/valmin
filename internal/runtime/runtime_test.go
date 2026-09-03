package runtime

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// Both implementations satisfy the seam. ADR-013 is only worth having if the fake is a
// real substitute, so this is a compile-time assertion rather than a runtime one.
var (
	_ Runtime = (*Docker)(nil)
	_ Runtime = (*Fake)(nil)
)

// TestAdapterKnowsNothingAboveIt enforces ADR-013. The adapter wraps a container engine;
// a dependency on the instance manager, the store or the API would make it a second
// place where instance semantics live.
func TestAdapterKnowsNothingAboveIt(t *testing.T) {
	forbidden := []string{
		"github.com/valminhq/valmin/internal/instance",
		"github.com/valminhq/valmin/internal/store",
		"github.com/valminhq/valmin/internal/api",
		"github.com/valminhq/valmin/internal/jobs",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range file.Imports {
			got := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if got == bad || strings.HasPrefix(got, bad+"/") {
					t.Errorf("%s imports %s", e.Name(), got)
				}
			}
		}
	}
}

func TestFakeLifecycle(t *testing.T) {
	f := NewFake()
	ctx := t.Context()

	id, err := f.Create(ctx, &ContainerSpec{
		User:   testContainerUser,
		Name:   "valmin-abc",
		Image:  "valmin/valheim:dev",
		Labels: map[string]string{"io.valmin.managed": "true", "io.valmin.instance.id": "abc"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	c, err := f.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if c.Running {
		t.Error("a created container is not yet running")
	}

	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if c, _ = f.Inspect(ctx, id); !c.Running || c.StartedAt.IsZero() {
		t.Errorf("after start: running=%v started_at=%v", c.Running, c.StartedAt)
	}

	if err := f.Stop(ctx, id, "SIGINT", 120*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	code, err := f.Wait(ctx, id)
	if err != nil || code != 0 {
		t.Fatalf("wait after stop: code=%d err=%v", code, err)
	}

	if err := f.Remove(ctx, id, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := f.Inspect(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("inspect after remove: got %v, want ErrNotFound", err)
	}
}

// The fake must be able to produce the case 08 §6's OOM guard exists for, or that guard
// has no test until someone has a machine willing to OOM-kill a real container.
func TestFakeScriptsAnOOMKill(t *testing.T) {
	f := NewFake()
	f.OnStart = func(c *FakeContainer) { c.OOMKill() }
	ctx := t.Context()

	id, err := f.Create(ctx, &ContainerSpec{User: testContainerUser, Image: "valmin/valheim:dev"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	c, err := f.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !c.OOMKilled {
		t.Error("OOMKilled is false")
	}
	if c.Running {
		t.Error("an OOM-killed container is not running")
	}
	if c.ExitCode != 137 {
		t.Errorf("exit code %d, want 137", c.ExitCode)
	}
}

func TestFakeScriptsRestartCount(t *testing.T) {
	f := NewFake()
	f.OnStart = func(c *FakeContainer) { c.RestartCount = 7 }
	ctx := t.Context()

	id, _ := f.Create(ctx, &ContainerSpec{User: testContainerUser, Image: "x"})
	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if c, _ := f.Inspect(ctx, id); c.RestartCount != 7 {
		t.Errorf("restart count %d, want 7", c.RestartCount)
	}
}

func TestFakeListFiltersByLabel(t *testing.T) {
	f := NewFake()
	ctx := t.Context()

	mine, _ := f.Create(
		ctx,
		&ContainerSpec{User: testContainerUser, Image: "x", Labels: map[string]string{"io.valmin.managed": "true"}},
	)
	_, _ = f.Create(
		ctx,
		&ContainerSpec{User: testContainerUser, Image: "x", Labels: map[string]string{"com.example": "other"}},
	)
	_, _ = f.Create(ctx, &ContainerSpec{User: testContainerUser, Image: "x"})

	got, err := f.List(ctx, map[string]string{"io.valmin.managed": "true"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != mine {
		t.Fatalf("got %d containers %v, want just %s", len(got), got, mine)
	}
}

func TestFakeWaitRespectsContext(t *testing.T) {
	f := NewFake()
	ctx := t.Context()

	id, _ := f.Create(ctx, &ContainerSpec{User: testContainerUser, Image: "x"})
	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Wait(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Errorf("wait on a cancelled context: got %v, want context.Canceled", err)
	}
}

// RunThrowaway is the mechanism behind the host_data_root self-check, whose whole job is
// to read one token off stdout. Separating the streams is therefore load-bearing: a
// warning on stderr must not end up in the token.
func TestRunThrowawaySeparatesTheStreams(t *testing.T) {
	f := NewFake()
	f.OnStart = func(c *FakeContainer) {
		c.Stdout("a-token\n")
		c.Stderr("a warning\n")
		c.Exit(0)
	}

	var out, errOut bytes.Buffer
	code, err := RunThrowaway(t.Context(), f, &ThrowawaySpec{
		User:      testContainerUser,
		Image:     "busybox",
		Cmd:       []string{"cat", "/check/.valmin-hostcheck"},
		NoNetwork: true,
		Stdout:    &out,
		Stderr:    &errOut,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	if got := out.String(); got != "a-token\n" {
		t.Errorf("stdout %q, want %q", got, "a-token\n")
	}
	if got := errOut.String(); got != "a warning\n" {
		t.Errorf("stderr %q, want %q", got, "a warning\n")
	}
}

func TestRunThrowawayReportsANonZeroExitWithoutAnError(t *testing.T) {
	f := NewFake()
	f.OnStart = func(c *FakeContainer) { c.Exit(42) }

	code, err := RunThrowaway(t.Context(), f, &ThrowawaySpec{User: testContainerUser, Image: "busybox"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 42 {
		t.Errorf("exit code %d, want 42", code)
	}
}

func TestRunThrowawayAlwaysRemovesTheContainer(t *testing.T) {
	f := NewFake()
	f.OnStart = func(c *FakeContainer) { c.Exit(0) }

	if _, err := RunThrowaway(t.Context(), f, &ThrowawaySpec{User: testContainerUser, Image: "busybox"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	left, err := f.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d containers left behind: %v", len(left), left)
	}
}

// A self-check that times out must not leak the container it started, or every restart
// of a misconfigured panel adds one more (10 §1.2).
func TestRunThrowawayRemovesTheContainerAfterCancellation(t *testing.T) {
	f := NewFake()
	ctx, cancel := context.WithCancel(t.Context())
	f.OnStart = func(_ *FakeContainer) { cancel() }

	if _, err := RunThrowaway(ctx, f, &ThrowawaySpec{User: testContainerUser, Image: "busybox"}); err == nil {
		t.Fatal("want an error from the cancelled wait")
	}
	left, err := f.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d containers left behind: %v", len(left), left)
	}
}

// E11, Q24: docker stats reports memory.current less inactive_file, and the cgroup v1
// key is spelled differently. Measured at 1122111488 − 1204224 = 1120907264.
func TestWorkingSet(t *testing.T) {
	tests := []struct {
		name  string
		usage uint64
		stats map[string]uint64
		want  uint64
	}{
		{
			name:  "cgroup v2, the measured sample",
			usage: 1122111488,
			stats: map[string]uint64{"inactive_file": 1204224},
			want:  1120907264,
		},
		{
			name:  "cgroup v1 spells it total_inactive_file",
			usage: 1122111488,
			stats: map[string]uint64{"total_inactive_file": 1204224},
			want:  1120907264,
		},
		{
			name:  "v2 key wins when both are present",
			usage: 100,
			stats: map[string]uint64{"inactive_file": 10, "total_inactive_file": 90},
			want:  90,
		},
		{
			name:  "no cache term reported",
			usage: 100,
			stats: nil,
			want:  100,
		},
		{
			name:  "cache larger than usage does not wrap",
			usage: 10,
			stats: map[string]uint64{"inactive_file": 99},
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workingSet(container.MemoryStats{Usage: tt.usage, Stats: tt.stats})
			if got != tt.want {
				t.Errorf("workingSet = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseTime(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantZero bool
		wantErr  bool
	}{
		{name: "a real timestamp", in: "2026-08-20T08:17:58.123456789Z"},
		{name: "docker's never", in: "0001-01-01T00:00:00Z", wantZero: true},
		{name: "empty", in: "", wantZero: true},
		{name: "not a timestamp", in: "yesterday", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTime(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTime(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTime(%q): %v", tt.in, err)
			}
			if got.IsZero() != tt.wantZero {
				t.Errorf("parseTime(%q) = %v, zero=%v, want zero=%v", tt.in, got, got.IsZero(), tt.wantZero)
			}
		})
	}
}

func TestBinds(t *testing.T) {
	got := binds([]Bind{
		{HostPath: "/srv/valmin/instances/a/worlds", ContainerPath: "/opt/valheim/worlds"},
		{HostPath: "/srv/valmin", ContainerPath: "/check", ReadOnly: true},
	})
	want := []string{
		"/srv/valmin/instances/a/worlds:/opt/valheim/worlds:rw",
		"/srv/valmin:/check:ro",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bind %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPortMapsPublishesTheRequestedProtocol(t *testing.T) {
	exposed, bindings := portMaps([]Port{{HostPort: 2456, ContainerPort: 2456, Proto: "udp"}})

	if _, ok := exposed["2456/udp"]; !ok {
		t.Errorf("exposed = %v, want 2456/udp", exposed)
	}
	got := bindings["2456/udp"]
	if len(got) != 1 || got[0].HostPort != "2456" {
		t.Errorf("bindings = %v, want host port 2456", bindings)
	}
}

func TestPortMapsAreNilWhenNothingIsPublished(t *testing.T) {
	exposed, bindings := portMaps(nil)
	if exposed != nil || bindings != nil {
		t.Errorf("got %v / %v, want nil / nil", exposed, bindings)
	}
}

// testContainerUser is what a test container states it runs as. Tests name a uid for
// the same reason production does: ContainerSpec.Validate refuses a spec that does not, and
// a test double that quietly skipped the rule would be the one place the guard is missing —
// which is exactly how the defect it exists for reached production.
const testContainerUser = "10000:10000"

// TestCreateRefusesASpecWithNoUser is the guard for the defect class described on
// ContainerSpec.Validate: a spec that says nothing about its uid silently takes the image's,
// which is root for most images — and since every container here drops all capabilities
// (08 §5), that root cannot write the panel's own directories. The symptom surfaces as
// `Permission denied` inside a container, far from the line that forgot the field.
//
// Asserted against the fake, deliberately. The path that shipped this bug was unit-tested
// with the fake and integration-tested by a test that expects failure on any host that is
// not uid 10000, so the fast suite is exactly where the guard has to bite.
func TestCreateRefusesASpecWithNoUser(t *testing.T) {
	f := NewFake()
	if _, err := f.Create(t.Context(), &ContainerSpec{Image: "steamcmd/steamcmd:latest"}); err == nil {
		t.Fatal("Create accepted a spec with no User; it must refuse one (08 §2)")
	}
	if _, err := f.Create(t.Context(), &ContainerSpec{
		Image: "steamcmd/steamcmd:latest", User: testContainerUser,
	}); err != nil {
		t.Fatalf("Create refused a spec that names its uid: %v", err)
	}
}
