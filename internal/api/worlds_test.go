package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
)

// The endpoint exists to answer the question a refused backup raises and cannot itself answer:
// the pair it wanted is not in the archive — is the world under another name, in another
// directory, or not there at all? Each wants a different move from the operator.
func TestListWorldsShowsWhatIsActuallyOnDisk(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	root := worldsDirOf(t, db)

	local := filepath.Join(root, instance.WorldsLocalDir)
	if err := os.MkdirAll(local, 0o775); err != nil {
		t.Fatal(err)
	}
	// The world this instance is configured to load, a world saved under another name, and
	// half a pair — the shape a failed import leaves behind.
	write := func(path string, size int) {
		t.Helper()
		if err := os.WriteFile(path, make([]byte, size), 0o664); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(local, "World.db"), 2048)
	write(filepath.Join(local, "World.fwl"), 32)
	write(filepath.Join(local, "Midgard.db"), 4096)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/worlds", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET worlds = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page Page[worldView]
	decodeInto(t, rec, &page)

	byName := map[string]worldView{}
	for _, world := range page.Items {
		byName[world.Name] = world
	}
	if len(byName) != 2 {
		t.Fatalf("read back %d worlds, want 2: %+v", len(byName), page.Items)
	}

	loaded := byName["World"]
	if !loaded.Loaded || !loaded.Complete {
		t.Errorf("the instance's own world reads loaded=%v complete=%v", loaded.Loaded, loaded.Complete)
	}
	if loaded.DBBytes == nil || *loaded.DBBytes != 2048 {
		t.Errorf("db_bytes = %v, want 2048", loaded.DBBytes)
	}
	if loaded.Dir != instance.WorldsLocalDir {
		t.Errorf("dir = %q, want %q", loaded.Dir, instance.WorldsLocalDir)
	}

	other := byName["Midgard"]
	if other.Loaded {
		t.Error("a world this instance does not load is marked loaded")
	}
	if other.Complete {
		t.Error("half a pair is reported as a complete world")
	}
	if other.FWLBytes != nil {
		t.Errorf("fwl_bytes = %v, want null: the file is absent, not empty", other.FWLBytes)
	}
}

// An instance that has never run has written no world, and that is an empty list rather than
// an error: it is the honest answer and the one a fresh instance gives.
func TestListWorldsOnAnInstanceThatNeverRan(t *testing.T) {
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/worlds", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET worlds = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page Page[worldView]
	decodeInto(t, rec, &page)
	if len(page.Items) != 0 {
		t.Errorf("read back %+v, want no worlds", page.Items)
	}

	// A viewer holds backups.list, and an instance with no grant is 404 rather than 403.
	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/worlds", http.NoBody)); rec.Code != http.StatusOK {
		t.Errorf("viewer GET = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-nope/worlds", http.NoBody,
	)); rec.Code != http.StatusNotFound {
		t.Errorf("unseen instance = %d, want 404", rec.Code)
	}
}
