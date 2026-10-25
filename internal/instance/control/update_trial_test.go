package control

import (
	"errors"
	"reflect"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

func installedMod(fullName, version string) store.InstanceMod {
	return store.InstanceMod{FullName: fullName, Version: version, Source: source.Thunderstore}
}

func TestDiffModsListsOnlyChangedVersions(t *testing.T) {
	t.Parallel()
	from := []store.InstanceMod{
		installedMod("A-Same", "1.0.0"), installedMod("B-Moved", "1.0.0"), installedMod("D-Gone", "2.0.0"),
	}
	copied := []store.InstanceMod{
		installedMod("A-Same", "1.0.0"), installedMod("B-Moved", "1.1.0"), installedMod("C-New", "3.0.0"),
	}
	want := []manager.UpdateTarget{
		{FullName: "B-Moved", Source: "thunderstore", FromVersion: "1.0.0", Version: "1.1.0"},
	}
	if got := diffMods(from, copied); !reflect.DeepEqual(got, want) {
		t.Errorf("diffMods = %+v, want %+v", got, want)
	}
}

// TestDiffModsLeavesOutAPackageTheSourceMovedPast asserts an apply never puts back an older
// version than the server runs, as after the server was updated on its own since the test.
func TestDiffModsLeavesOutAPackageTheSourceMovedPast(t *testing.T) {
	t.Parallel()
	from := []store.InstanceMod{installedMod("A-Mod", "1.3.0")}
	copied := []store.InstanceMod{installedMod("A-Mod", "1.2.0")}
	if got := diffMods(from, copied); len(got) != 0 {
		t.Errorf("diffMods over a downgrade = %+v, want nothing", got)
	}
}

func TestGameNewer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		source, copy string
		want         bool
	}{
		{"newer", "21981590", "22110001", true},
		{"same", "21981590", "21981590", false},
		{"older, as when the server moved on after the test", "22110001", "21981590", false},
		{"not numbers but different", "a", "b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := TestChanges{SourceBuildID: tt.source, CopyBuildID: tt.copy}
			if got := c.GameNewer(); got != tt.want {
				t.Errorf("GameNewer(source %q, copy %q) = %v, want %v", tt.source, tt.copy, got, tt.want)
			}
		})
	}
}

func TestTestCPULimit(t *testing.T) {
	t.Parallel()
	limited, tiny := 6.0, 1.5
	tests := []struct {
		name string
		cpus int
		src  *float64
		want float64
	}{
		{"half of the server's limit", 16, &limited, 3},
		{"half of the host without a limit", 8, nil, 4},
		{"never below one core", 16, &tiny, 1},
		{"one-core host", 1, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tester := &UpdateTester{CPUs: tt.cpus}
			got := tester.testCPULimit(&store.Instance{CPULimit: tt.src})
			if *got != tt.want {
				t.Errorf("testCPULimit(%v) on %d cores = %v, want %v", tt.src, tt.cpus, *got, tt.want)
			}
		})
	}
}

func TestCheckMemoryRefusesACopyTheHostCannotHold(t *testing.T) {
	t.Parallel()
	src := &store.Instance{ID: "src", MemLimitMB: 4096}
	tests := []struct {
		name      string
		available func() (int, error)
		wantErr   bool
	}{
		{"enough", func() (int, error) { return 8192, nil }, false},
		{"exactly enough", func() (int, error) { return 4096, nil }, false},
		{"short", func() (int, error) { return 2048, nil }, true},
		{"unreadable is let through", func() (int, error) { return 0, errors.New("no meminfo") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tester := &UpdateTester{AvailableMemoryMB: tt.available}
			err := tester.checkMemory(t.Context(), src)
			var low *LowMemoryError
			if got := errors.As(err, &low); got != tt.wantErr {
				t.Fatalf("checkMemory = %v, want a LowMemoryError: %v", err, tt.wantErr)
			}
			if tt.wantErr && (low.AvailableMB != 2048 || low.NeededMB != 4096) {
				t.Errorf("LowMemoryError = %+v, want 2048 available and 4096 needed", low)
			}
		})
	}
}

func TestApplyReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload UpdateApplyPayload
		running bool
		want    string
	}{
		{
			"both, server running",
			UpdateApplyPayload{BuildID: "123", Mods: make([]manager.UpdateTarget, 2)},
			true,
			"applied game build 123 and 2 mod updates; the server is running",
		},
		{
			"mods only, server stopped",
			UpdateApplyPayload{Mods: make([]manager.UpdateTarget, 1)},
			false,
			"applied 1 mod updates; the server is stopped",
		},
		{"nothing", UpdateApplyPayload{}, false, "no changes were needed; the server is stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := applyReport(&tt.payload, tt.running); got != tt.want {
				t.Errorf("applyReport = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSubmitCloneTakesTheNextNameWhenOneIsTaken asserts a copy whose usual name belongs to
// another server is queued under that name with a number appended.
func TestSubmitCloneTakesTheNextNameWhenOneIsTaken(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	seed(t, w.db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-b', 'inst-a update test', 'stopped', '/srv/b', 2556, 'Server', 'World', 'x',
		'cp-inst-b', ?, ?)`, store.Now(), store.Now())
	src, err := w.c.Chainer.fresh(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	copyID := store.NewID()
	run := &CloneRun{
		Source: src, UpdateTest: true, ArchiveID: store.NewID(), ArchivePath: t.TempDir() + "/seed.tar.gz",
		Destination: &store.Instance{
			ID: copyID, Name: "inst-a update test", DataDir: instance.DataDir(w.cfg.Data.Root, copyID),
		},
	}

	if _, err := w.c.UpdateTester.submitClone(t.Context(), run); err != nil {
		t.Fatalf("submitClone = %v, want the copy queued", err)
	}
	if want := "inst-a update test 2"; run.Destination.Name != want {
		t.Errorf("copy named %q, want %q", run.Destination.Name, want)
	}
}
