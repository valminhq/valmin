package diag

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
)

// fakeEngine answers the three questions a report asks of the container engine.
type fakeEngine struct {
	pingErr error
	images  map[string]bool
	imgErr  error
}

func (f *fakeEngine) Ping(context.Context) error { return f.pingErr }
func (f *fakeEngine) APIVersion() string         { return "1.47" }
func (f *fakeEngine) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.images[ref], f.imgErr
}

// fakeIndex answers the mod index reachability probe.
type fakeIndex struct{ err error }

func (f fakeIndex) Reachable(context.Context, string) error { return f.err }

func testInput() *Input {
	cfg := config.Defaults()
	cfg.Server.ExternalURL = "https://valmin.example"
	cfg.Data.Root = "/srv/valmin"
	return &Input{
		Config: &cfg,
		Runtime: &fakeEngine{images: map[string]bool{
			cfg.Game.Image: true, cfg.Game.SteamCMDImage: true,
		}},
		Packages:           fakeIndex{},
		Hexium:             fakeIndex{},
		ThunderstoreSynced: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		HexiumSynced:       time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		Now:                time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		UID:                10000,
		GID:                10000,
		FSType:             "ext4",
		FreeBytes:          50 << 30,
		AlarmBytes:         2 << 30,
	}
}

// find returns the check with the given id, failing if the report has none.
func find(t *testing.T, r *Report, id string) Check {
	t.Helper()
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return r.Checks[i]
		}
	}
	t.Fatalf("report has no check %q", id)
	return Check{}
}

// TestAHealthyPanelPassesEveryProbedCheck asserts the baseline: nothing fails and nothing
// warns when the engine answers, the images are present and there is room.
func TestAHealthyPanelPassesEveryProbedCheck(t *testing.T) {
	t.Parallel()

	r := Collect(t.Context(), testInput())
	for _, c := range r.Checks {
		switch c.ID {
		case CheckHostRoot, CheckGameNetwork, CheckDataRootWrite, CheckSteam:
			if c.Status != StatusUnknown {
				t.Errorf("%s: got %s, want unknown with nothing recorded", c.ID, c.Status)
			}
		default:
			if c.Status != StatusOK {
				t.Errorf("%s: got %s (%s), want ok", c.ID, c.Status, c.Detail)
			}
		}
	}
	if r.Worst() != StatusUnknown {
		t.Errorf("worst = %s, want unknown", r.Worst())
	}
}

// TestFailuresAreReportedAsRowsRatherThanAbortingTheRun asserts that one broken
// dependency does not cost the report every other answer.
func TestFailuresAreReportedAsRowsRatherThanAbortingTheRun(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Runtime = &fakeEngine{pingErr: errors.New("socket refused")}
	in.Packages = fakeIndex{err: errors.New("no route to host")}

	r := Collect(t.Context(), in)
	if got := find(t, &r, CheckDockerPing); got.Status != StatusFail {
		t.Errorf("docker ping = %s, want fail", got.Status)
	}
	if got := find(t, &r, CheckThunderstore); got.Status != StatusFail {
		t.Errorf("thunderstore = %s, want fail", got.Status)
	}
	if got := find(t, &r, CheckFreeSpace); got.Status != StatusOK {
		t.Errorf("free space = %s, want ok despite the other failures", got.Status)
	}
	if r.Worst() != StatusFail {
		t.Errorf("worst = %s, want fail", r.Worst())
	}
}

func TestChecksReportTheirVerdict(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		id     string
		mutate func(*Input)
		want   Status
	}{
		"a missing game image fails": {
			id:     CheckGameImage,
			mutate: func(in *Input) { in.Runtime = &fakeEngine{} },
			want:   StatusFail,
		},
		"an engine that refuses image inspection fails": {
			id:     CheckSteamCMDImage,
			mutate: func(in *Input) { in.Runtime = &fakeEngine{imgErr: errors.New("denied")} },
			want:   StatusFail,
		},
		"free space below the floor fails": {
			id:     CheckFreeSpace,
			mutate: func(in *Input) { in.FreeBytes = 1 << 20 },
			want:   StatusFail,
		},
		"a uid other than 10000 warns": {
			id:     CheckIdentity,
			mutate: func(in *Input) { in.UID, in.GID = 1000, 1000 },
			want:   StatusWarn,
		},
		"plain http on a routable host warns": {
			id:     CheckExternalURL,
			mutate: func(in *Input) { in.Config.Server.ExternalURL = "http://192.168.1.10:8080" },
			want:   StatusWarn,
		},
		"plain http on localhost passes": {
			id:     CheckExternalURL,
			mutate: func(in *Input) { in.Config.Server.ExternalURL = "http://localhost:8080" },
			want:   StatusOK,
		},
		"an unprobed filesystem is unknown": {
			id:     CheckFilesystem,
			mutate: func(in *Input) { in.FSType = "" },
			want:   StatusUnknown,
		},
		"no mod index client leaves reachability unknown": {
			id:     CheckThunderstore,
			mutate: func(in *Input) { in.Packages = nil },
			want:   StatusUnknown,
		},
		"a recorded failure is reported with its detail": {
			id: CheckHostRoot,
			mutate: func(in *Input) {
				in.Recorded = map[string]Observation{
					CheckHostRoot: {OK: false, Detail: "token mismatch", CheckedAt: in.Now},
				}
			},
			want: StatusFail,
		},
		"a recorded pass is reported": {
			id: CheckGameNetwork,
			mutate: func(in *Input) {
				in.Recorded = map[string]Observation{
					CheckGameNetwork: {OK: true, CheckedAt: in.Now},
				}
			},
			want: StatusOK,
		},
		"a recorded steam observation passes": {
			id: CheckSteam,
			mutate: func(in *Input) {
				in.SteamBuildID, in.SteamObservedAt = "19425060", in.Now
			},
			want: StatusOK,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := testInput()
			tc.mutate(in)
			r := Collect(t.Context(), in)
			if got := find(t, &r, tc.id); got.Status != tc.want {
				t.Errorf("%s = %s (%s), want %s", tc.id, got.Status, got.Detail, tc.want)
			}
		})
	}
}

// TestAFailingCheckCarriesARemedy asserts an operator is never told only that something
// is broken.
func TestAFailingCheckCarriesARemedy(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Runtime = &fakeEngine{pingErr: errors.New("socket refused")}
	in.FreeBytes = 1 << 20
	in.UID = 1000
	in.Config.Server.ExternalURL = "http://192.168.1.10:8080"

	for _, c := range Collect(t.Context(), in).Checks {
		if c.Status == StatusFail || c.Status == StatusWarn {
			if c.Remedy == "" {
				t.Errorf("%s is %s but names no remedy", c.ID, c.Status)
			}
		}
	}
}

// TestPortsCompareAllocationAgainstWhatTheEnginePublishes asserts the mismatch is found
// and named, and that a stopped instance is not judged on ports it does not have.
func TestPortsCompareAllocationAgainstWhatTheEnginePublishes(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		instances []Instance
		want      Status
	}{
		"agreement passes": {
			instances: []Instance{{
				Name: "one", Running: true, BasePort: 2456,
				ExpectedPorts: []int{2456, 2457}, BoundPorts: []int{2457, 2456},
			}},
			want: StatusOK,
		},
		"a missing published port fails": {
			instances: []Instance{{
				Name: "one", Running: true, BasePort: 2456,
				ExpectedPorts: []int{2456, 2457}, BoundPorts: []int{2456},
			}},
			want: StatusFail,
		},
		"a stopped instance is not judged": {
			instances: []Instance{{
				Name: "one", Running: false, BasePort: 2456,
				ExpectedPorts: []int{2456, 2457},
			}},
			want: StatusOK,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := testInput()
			in.Instances = tc.instances
			r := Collect(t.Context(), in)
			if got := find(t, &r, CheckPortPublication); got.Status != tc.want {
				t.Errorf("ports = %s (%s), want %s", got.Status, got.Detail, tc.want)
			}
		})
	}
}

// TestThePortsCheckDisclaimsExternalReachability asserts the copy an operator reads does
// not let a pass be mistaken for "players can connect" (03 §2).
func TestThePortsCheckDisclaimsExternalReachability(t *testing.T) {
	t.Parallel()

	r := Collect(t.Context(), testInput())
	got := find(t, &r, CheckPortPublication)
	if !strings.Contains(got.Detail, "does not test reachability from the internet") {
		t.Errorf("ports detail does not disclaim external reachability: %q", got.Detail)
	}
}

// TestAnUnreachableEngineLeavesDockerChecksUnknown asserts the command-line report, which
// runs with no engine at all, does not render four passes.
func TestAnUnreachableEngineLeavesDockerChecksUnknown(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Runtime = nil

	r := Collect(t.Context(), in)
	for _, id := range []string{CheckDockerPing, CheckDockerAPI, CheckGameImage, CheckSteamCMDImage} {
		if got := find(t, &r, id); got.Status != StatusUnknown {
			t.Errorf("%s = %s, want unknown with no engine", id, got.Status)
		}
	}
}

func TestRegistryHealthSeparatesReachabilityAndRefresh(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                           string
		disabled, failed, stale, never bool
		want                           Status
	}{
		{name: "healthy", want: StatusOK},
		{name: "failed refresh", failed: true, want: StatusFail},
		{name: "stale index", stale: true, want: StatusWarn},
		{name: "never synced", never: true, want: StatusUnknown},
		{name: "disabled", disabled: true, want: StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			in.Config.Hexium.Enabled = !tc.disabled
			if tc.failed {
				in.RegistrySyncs = map[string]RegistrySync{
					CheckHexium: {CheckedAt: in.Now, Error: "private-host /private/path", OK: false},
				}
			}
			if tc.stale {
				in.HexiumSynced = in.Now.Add(-3 * in.Config.Thunderstore.SyncInterval.Std())
			}
			if tc.never {
				in.HexiumSynced = time.Time{}
			}
			if tc.disabled {
				in.Hexium = fakeIndex{err: errors.New("must not probe disabled registry")}
			}
			r := Collect(t.Context(), in)
			id := CheckHexium + ".sync"
			if tc.disabled {
				id = CheckHexium
			}
			if got := find(t, &r, id); got.Status != tc.want {
				t.Fatalf("%s = %s, want %s", id, got.Status, tc.want)
			}
			if got := find(t, &r, CheckThunderstore); got.Status != StatusOK {
				t.Fatalf("Thunderstore = %s", got.Status)
			}
			if got := find(t, &r, CheckHexium); got.Status != StatusOK {
				t.Fatalf("Hexium reachability = %s", got.Status)
			}
		})
	}
}

func TestFailedInspectionDoesNotClaimMissingPorts(t *testing.T) {
	t.Parallel()
	in := testInput()
	in.Instances = []Instance{
		{Running: true, ExpectedPorts: []int{2456, 2457}, InspectionError: "Docker refused inspection"},
	}
	r := Collect(t.Context(), in)
	if got := find(t, &r, CheckPortPublication); got.Status != StatusUnknown {
		t.Fatalf("ports = %s, want unknown", got.Status)
	}
	if r.Instances[0].PortIssue != "" {
		t.Fatal("an inspection failure was reported as a port mismatch")
	}
}

func TestBundleOmitsInstanceReadErrors(t *testing.T) {
	t.Parallel()
	r := Report{
		Instances: []Instance{{ModsError: "/private/mods", InspectionError: "private-docker-host"}},
		Checks:    []Check{{Diagnostic: "/private/registry"}},
	}
	out := r.forBundle()
	if strings.Contains(out.Instances[0].ModsError, "/private") ||
		strings.Contains(out.Instances[0].InspectionError, "private-docker") ||
		out.Checks[0].Diagnostic != "" {
		t.Fatal("bundle includes raw probe errors")
	}
	if r.Instances[0].ModsError != "/private/mods" {
		t.Fatal("redaction changed the live report")
	}
}
