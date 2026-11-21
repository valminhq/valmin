package scan

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

// modded is a modded instance whose BepInEx log is testdata/bepinex-failed.log, last written at
// written.
func modded(t *testing.T, id string, written time.Time) store.Instance {
	t.Helper()
	raw, err := os.ReadFile("testdata/bepinex-failed.log")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "server", "BepInEx", "LogOutput.log")
	if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(log, written, written); err != nil {
		t.Fatal(err)
	}
	return store.Instance{ID: id, DataDir: dir, Modded: true}
}

// TestFailedModsReadOnlyTheLatestBoot asserts the loader's failures are reported for the boot
// that wrote them, and a log older than the latest start says nothing.
func TestFailedModsReadOnlyTheLatestBoot(t *testing.T) {
	t.Parallel()
	written := time.Now().Add(-time.Hour)
	before, after := written.Add(-time.Minute), written.Add(time.Minute)
	for _, tc := range []struct {
		name      string
		container *time.Time
		jobStart  *time.Time
		want      bool
	}{
		{"log written after the container started", &before, nil, true},
		{"container started after the log", &after, nil, false},
		{"a start job the observer has not seen yet", &before, &after, false},
		{"no recorded boot", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst := modded(t, "a", written)
			inst.ContainerStartedAt = tc.container
			var latest []store.Job
			if tc.jobStart != nil {
				latest = []store.Job{{
					Kind: jobs.KindStart.String(), Status: jobs.StatusSucceeded,
					InstanceID: &inst.ID, StartedAt: tc.jobStart,
				}}
			}
			s := &Scanner{Patterns: instance.DefaultPatterns}
			got, ok := s.failedMods(t.Context(), []store.Instance{inst}, latest)["a"]
			if ok != tc.want {
				t.Fatalf("reported = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			want := []string{"Jotunn", "OdinArchitect (skipped)", "PlanBuild (skipped)"}
			if !slices.Equal(got.Names, want) || got.Reason == "" {
				t.Errorf("failed mods = %+v, want %v with a reason", got, want)
			}
		})
	}
}

// TestModUpdatesAreReadOnlyWhereARuleAsks asserts the catalogue is asked about a server only
// when an enabled mod_updates rule covers it.
func TestModUpdatesAreReadOnlyWhereARuleAsks(t *testing.T) {
	t.Parallel()
	a, b := "a", "b"
	instances := []store.Instance{{ID: a}, {ID: b}}
	kind := alerts.KindModUpdates.String()
	for _, tc := range []struct {
		name  string
		rules []store.AlertRule
		want  []string
	}{
		{"no rule", nil, nil},
		{"a disabled rule", []store.AlertRule{{ConditionKind: kind}}, nil},
		{"a rule for one server", []store.AlertRule{{ConditionKind: kind, Enabled: true, InstanceID: &b}}, []string{b}},
		{"a rule for every server", []store.AlertRule{{ConditionKind: kind, Enabled: true}}, []string{a, b}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			s := &Scanner{ModUpdates: func(_ context.Context, id string) ([]manager.UpdateTarget, error) {
				asked = append(asked, id)
				return []manager.UpdateTarget{{FullName: "Jotunn", FromVersion: "2.30.2", Version: "2.30.3"}}, nil
			}}
			got := s.modUpdates(t.Context(), tc.rules, instances)
			if !slices.Equal(asked, tc.want) {
				t.Errorf("asked about %v, want %v", asked, tc.want)
			}
			for _, id := range tc.want {
				if line := got[id]; len(line) != 1 || line[0] != "Jotunn 2.30.2 → 2.30.3" {
					t.Errorf("updates for %s = %v", id, line)
				}
			}
		})
	}
}
