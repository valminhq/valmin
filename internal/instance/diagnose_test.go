package instance

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// failedLoad parses testdata/bepinex-failed.log as written at observedAt.
func failedLoad(t *testing.T, observedAt time.Time) *PluginLoad {
	t.Helper()
	f, err := os.Open("testdata/bepinex-failed.log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	load := parsePluginLoad(f)
	load.ObservedAt = observedAt
	return &load
}

func codes(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for i := range findings {
		out = append(out, findings[i].Code)
	}
	return out
}

// TestDiagnoseReportsWhatTheBootObserved asserts which findings each kind of boot produces, and
// their order.
func TestDiagnoseReportsWhatTheBootObserved(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	fresh := started.Add(time.Minute)
	stale := started.Add(-time.Hour)
	running := &runtime.Container{Running: true, StartedAt: started}
	exited := &runtime.Container{ExitCode: 1, StartedAt: started}
	oom := &runtime.Container{ExitCode: 137, OOMKilled: true, StartedAt: started}
	notReady := &BootScan{LastLine: "Loading world"}
	ready := &BootScan{Ready: true}

	tests := []struct {
		name string
		ev   Evidence
		want []string
	}{
		{"a healthy vanilla server", Evidence{State: StateRunning, Container: running}, nil},
		{"a never-provisioned server", Evidence{State: StateCreated}, nil},
		{
			"a modded server whose mods loaded",
			Evidence{
				State: StateRunning, Modded: true, Container: running,
				Plugins: &PluginLoad{Declared: 0, ObservedAt: fresh},
			},
			nil,
		},
		{
			"an out-of-memory kill",
			Evidence{State: StateError, Container: oom, Boot: notReady},
			[]string{"oom_killed"},
		},
		{
			"failed mods on a running server",
			Evidence{State: StateRunning, Modded: true, Container: running, Plugins: failedLoad(t, fresh)},
			[]string{"plugin_failed"},
		},
		{
			"failed mods and an exit before ready",
			Evidence{
				State: StateError, Modded: true, Container: exited, Boot: notReady,
				Plugins: failedLoad(t, fresh),
			},
			[]string{"plugin_failed", "exited_early"},
		},
		{
			"a vanilla exit before ready",
			Evidence{State: StateError, Container: exited, Boot: notReady},
			[]string{"exited_early"},
		},
		{
			"an exit after ready is not an early exit",
			Evidence{State: StateError, Container: exited, Boot: ready},
			nil,
		},
		{
			"a readiness timeout",
			Evidence{State: StateError, Container: running, Boot: notReady},
			[]string{"not_ready"},
		},
		{
			"a modded server with no loader log",
			Evidence{State: StateRunning, Modded: true, Container: running},
			[]string{"loader_not_run"},
		},
		{
			"a loader log from an earlier boot proves nothing",
			Evidence{State: StateRunning, Modded: true, Container: running, Plugins: failedLoad(t, stale)},
			[]string{"loader_not_run"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codes(Diagnose(&tt.ev)); !slices.Equal(got, tt.want) {
				t.Errorf("Diagnose = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAFailedModIsOneRootWithItsCascadeFolded asserts a failure is named by its package, carries
// the loader's line, and lists the plugins skipped because of it rather than reporting them as
// failures of their own.
func TestAFailedModIsOneRootWithItsCascadeFolded(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	findings := Diagnose(&Evidence{
		State: StateRunning, Modded: true,
		Container: &runtime.Container{Running: true, StartedAt: started},
		Plugins:   failedLoad(t, started.Add(time.Minute)),
		Mods: []ModRef{{
			FullName: "ValheimModding-Jotunn", Deprecated: true,
			ManifestPaths: []string{"BepInEx/plugins/ValheimModding-Jotunn/Jotunn.dll"},
		}},
	})
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want one root", codes(findings))
	}
	f := findings[0]
	if f.Title != "ValheimModding-Jotunn did not load" {
		t.Errorf("title = %q", f.Title)
	}
	if !strings.HasPrefix(f.Evidence, "Error loading [Jotunn 2.30.2]") {
		t.Errorf("evidence = %q", f.Evidence)
	}
	if want := []string{"OdinArchitect 1.7.0", "PlanBuild 0.18.0"}; !slices.Equal(f.Related, want) {
		t.Errorf("related = %v, want %v", f.Related, want)
	}
	if !strings.Contains(strings.Join(f.Fixes, " "), "deprecated") {
		t.Errorf("fixes for a deprecated package = %v, want one saying so", f.Fixes)
	}
	if f.Target != TargetMods {
		t.Errorf("target = %q, want %q", f.Target, TargetMods)
	}
}

// TestAnEarlyExitQuotesTheFirstLoaderError asserts the quoted line is the loader's first error
// that names no plugin, and the console's last line when the loader recorded none.
func TestAnEarlyExitQuotesTheFirstLoaderError(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	ev := Evidence{
		State: StateError, Modded: true,
		Container: &runtime.Container{ExitCode: 1, StartedAt: started},
		Boot:      &BootScan{LastLine: "Loading world"},
		Plugins:   failedLoad(t, started.Add(time.Minute)),
	}
	got := Diagnose(&ev)
	early := got[len(got)-1]
	if want := "HarmonyException: Patching exception in method static System.Void Player::Awake()"; early.Evidence != want {
		t.Errorf("evidence = %q, want %q", early.Evidence, want)
	}

	ev.Plugins = nil
	ev.Modded = false
	got = Diagnose(&ev)
	if got[0].Evidence != "Loading world" || got[0].Target != "" {
		t.Errorf("vanilla early exit = %+v, want the console's last line and no target", got[0])
	}
}

// TestScanBootStopsAtReadiness asserts the scan reports readiness and the last line before it,
// ignoring what the server printed afterwards.
func TestScanBootStopsAtReadiness(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) {
		c.Stdout("Loading world\n\n08/20/2026 08:17:58: Game server connected\nlater line\n")
	}
	id := fakeContainer(t, rt)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c, err := rt.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := ScanBoot(t.Context(), rt, &c)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.Ready || scan.LastLine != "Loading world" {
		t.Errorf("ScanBoot = %+v, want ready after %q", scan, "Loading world")
	}
}
