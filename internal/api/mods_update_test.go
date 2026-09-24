package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// updatable is two explicit mods and a dependency, each with a newer version, one of which
// pulls in a package nothing had installed:
//
//	Ns-Only  1.0.0 -> 2.0.0
//	Ns-Other 1.0.0 -> 1.1.0, which now also needs Ns-New
//	Ns-Lib   1.0.0 -> 1.1.0, installed as Ns-Other's dependency
func updatable() []modPackageFixture {
	return append(twoVersions(),
		modPackageFixture{
			fullName: "Ns-Lib", version: "1.0.0",
			files: map[string]string{"manifest.json": "{}", "plugins/Lib.dll": "lib v1"},
		},
		modPackageFixture{
			fullName: "Ns-Lib", version: "1.1.0",
			files: map[string]string{"manifest.json": "{}", "plugins/Lib.dll": "lib v1.1"},
		},
		modPackageFixture{
			fullName: "Ns-Other", version: "1.0.0", deps: []string{"Ns-Lib-1.0.0"},
			files: map[string]string{"manifest.json": "{}", "plugins/Other.dll": "other v1"},
		},
		modPackageFixture{
			fullName: "Ns-Other", version: "1.1.0", deps: []string{"Ns-Lib-1.1.0", "Ns-New-1.0.0"},
			files: map[string]string{"manifest.json": "{}", "plugins/Other.dll": "other v1.1"},
		},
		modPackageFixture{
			fullName: "Ns-New", version: "1.0.0",
			files: map[string]string{"manifest.json": "{}", "plugins/New2.dll": "new"},
		},
	)
}

// updateWorld installs the old versions and gives the server a world to protect.
func updateWorld(t *testing.T) (rt *Router, db *store.DB, admin *store.User, dataDir string) {
	t.Helper()
	rt, db, admin, _, dataDir = installWorld(t, updatable()...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "Ns-Only", "1.0.0")
	installClosure(t, rt, admin, "Ns-Other", "1.0.0")
	worlds := filepath.Join(instance.WorldsDir(dataDir), "worlds_local")
	if err := os.MkdirAll(worlds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worlds, "World.db"), []byte("the world"), 0o644); err != nil {
		t.Fatal(err)
	}
	return rt, db, admin, dataDir
}

func previewUpdatesOf(t *testing.T, rt *Router, u *store.User) updatePreview {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodPost,
		"/api/v1/instances/inst-a/mods/updates/resolve", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", rec.Code, rec.Body)
	}
	var preview updatePreview
	decodeInto(t, rec, &preview)
	return preview
}

func postUpdates(t *testing.T, rt *Router, u *store.User, targets []updateTarget) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/updates",
		jsonBody(t, applyUpdatesRequest{Targets: targets})))
}

// TestUpdateAllShowsOneCombinedDiff asserts the preview is the whole change at once: every
// package moving, from which version to which, and the package the updates newly pull in.
func TestUpdateAllShowsOneCombinedDiff(t *testing.T) {
	rt, _, admin, _ := updateWorld(t)

	preview := previewUpdatesOf(t, rt, admin)
	if len(preview.Targets) != 3 {
		t.Fatalf("targets = %+v, want Ns-Only, Ns-Other and Ns-Lib", preview.Targets)
	}
	if !preview.Backup {
		t.Error("backup = false for a server with a world")
	}
	nodes := map[string]updateNode{}
	for _, n := range preview.Nodes {
		nodes[n.FullName] = n
	}
	for name, want := range map[string][2]string{
		"Ns-Only":  {"1.0.0", "2.0.0"},
		"Ns-Other": {"1.0.0", "1.1.0"},
		"Ns-Lib":   {"1.0.0", "1.1.0"},
		"Ns-New":   {"", "1.0.0"},
	} {
		n, ok := nodes[name]
		if !ok {
			t.Errorf("%s is missing from the diff %+v", name, preview.Nodes)
			continue
		}
		if n.FromVersion != want[0] || n.Version != want[1] || n.Source != "thunderstore" {
			t.Errorf("%s = %+v, want %s -> %s", name, n, want[0], want[1])
		}
	}
	if len(nodes) != 4 {
		t.Errorf("diff has %d rows, want the four packages that change", len(nodes))
	}
}

// TestUpdateAllBacksUpThenUpdatesEverything runs the confirmed diff: one job, an archive of the
// world before any file moved, every package at its new version, and each package keeping the
// reason it was installed.
func TestUpdateAllBacksUpThenUpdatesEverything(t *testing.T) {
	rt, db, admin, dataDir := updateWorld(t)

	rec := postUpdates(t, rt, admin, previewUpdatesOf(t, rt, admin).Targets)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apply = %d (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("update job = %+v, want succeeded", got)
	}

	rows := installedRows(t, db)
	for name, want := range map[string][2]string{
		"Ns-Only":  {"2.0.0", store.InstalledExplicit},
		"Ns-Other": {"1.1.0", store.InstalledExplicit},
		"Ns-Lib":   {"1.1.0", store.InstalledDependency},
		"Ns-New":   {"1.0.0", store.InstalledDependency},
	} {
		if got := rows[name]; got.Version != want[0] || got.InstalledAs != want[1] {
			t.Errorf("%s = %s/%s, want %s/%s", name, got.Version, got.InstalledAs, want[0], want[1])
		}
	}
	if body, err := os.ReadFile(serverPath(dataDir, "BepInEx/plugins/Lib.dll")); err != nil ||
		string(body) != "lib v1.1" {
		t.Errorf("Lib.dll = %q, %v; want the new version", body, err)
	}
	if _, err := os.Stat(serverPath(dataDir, "BepInEx/plugins/Gone.dll")); err == nil {
		t.Error("an update-all left a file the old version shipped and the new one dropped")
	}

	var trigger string
	var consistent bool
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT trigger, consistent FROM backups WHERE instance_id = 'inst-a'`).
		Scan(&trigger, &consistent); err != nil {
		t.Fatalf("no archive was recorded: %v", err)
	}
	if trigger != store.TriggerPreUpdate || !consistent {
		t.Errorf("archive = %s/%v, want a consistent pre_update archive", trigger, consistent)
	}

	if again := previewUpdatesOf(t, rt, admin); len(again.Targets) != 0 {
		t.Errorf("targets after updating everything = %+v, want none", again.Targets)
	}
}

// TestUpdateAllRefusesWhatItWasNotShown asserts the apply request is checked against what is
// installed now: no downgrade, nothing uninstalled, no change of registry, no empty request.
func TestUpdateAllRefusesWhatItWasNotShown(t *testing.T) {
	rt, _, admin, _ := updateWorld(t)

	for name, targets := range map[string][]updateTarget{
		"empty":            {},
		"not installed":    {{FullName: "Ns-New", Source: "thunderstore", Version: "1.0.0"}},
		"not newer":        {{FullName: "Ns-Only", Source: "thunderstore", Version: "1.0.0"}},
		"another registry": {{FullName: "Ns-Only", Source: "hexium", Version: "2.0.0"}},
		"named twice": {
			{FullName: "Ns-Only", Source: "thunderstore", Version: "2.0.0"},
			{FullName: "Ns-Only", Source: "thunderstore", Version: "2.0.0"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := postUpdates(t, rt, admin, targets); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("apply = %d (%s), want 422", rec.Code, rec.Body)
			}
		})
	}
}

// TestUpdateAllIsRefusedWhileTheServerRuns is B11 for the new route: the plugin directory is
// read once at startup, so changing it under a running server is refused like any install.
func TestUpdateAllIsRefusedWhileTheServerRuns(t *testing.T) {
	rt, db, admin, _ := updateWorld(t)
	targets := previewUpdatesOf(t, rt, admin).Targets
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)

	if rec := postUpdates(t, rt, admin, targets); rec.Code != http.StatusConflict {
		t.Errorf("apply on a running server = %d (%s), want 409", rec.Code, rec.Body)
	}
}

// TestTheArchiveIsRecordedWhateverTheOutcome asserts withArchive's contract: the archive's row
// lands with a failed outcome too, and ahead of the outcome's own finish work.
func TestTheArchiveIsRecordedWhateverTheOutcome(t *testing.T) {
	var order []string
	archived := func(context.Context, *sql.Tx) error { order = append(order, "archive"); return nil }

	failed := withArchive(jobs.Outcome{Status: jobs.StatusFailed}, archived)
	if failed.OnFinish == nil {
		t.Fatal("a failed update drops the archive it took")
	}
	if err := failed.OnFinish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	then := func(context.Context, *sql.Tx) error { order = append(order, "then"); return nil }
	ok := withArchive(jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: then}, archived)
	if err := ok.OnFinish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[1] != "archive" || order[2] != "then" {
		t.Errorf("order = %v, want the archive before the outcome's own finish", order)
	}
	if out := withArchive(jobs.Outcome{Status: jobs.StatusSucceeded}, nil); out.OnFinish != nil {
		t.Error("no archive still installed a finish hook")
	}
}
