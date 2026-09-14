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

// The layout build 25253791 writes: a directory named what `-world` names, holding the
// generation's halves beside the chunk files that hold most of the world
// (evidence/world-format-1.0-2026-09-14.md). Every world-shaped check in the panel read the
// pre-1.0 pair and reported a 1.0 world as absent, which is what made backups impossible.
func TestListWorldsReadsAOneZeroWorld(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	root := worldsDirOf(t, db)

	world := filepath.Join(root, instance.WorldsLocalDir, "World")
	rolling := filepath.Join(root, instance.WorldsLocalDir, "World_backup_auto-20260913-204651")
	for _, dir := range []string{world, rolling} {
		if err := os.MkdirAll(dir, 0o775); err != nil {
			t.Fatal(err)
		}
		for name, size := range map[string]int{
			"_main.14.db2": 4096, "_main.14.fwl2": 113,
			"_main.14.chunks": 65, "_main.14.ok": 4, "20_20__1_9.chunk": 8192,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o664); err != nil {
				t.Fatal(err)
			}
		}
	}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/worlds", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET worlds = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page Page[worldView]
	decodeInto(t, rec, &page)

	byName := map[string]worldView{}
	for _, w := range page.Items {
		byName[w.Name] = w
	}
	loaded, ok := byName["World"]
	if !ok {
		t.Fatalf("the 1.0 world was not listed: %+v", page.Items)
	}
	if !loaded.Loaded || !loaded.Complete {
		t.Errorf("loaded=%v complete=%v, want both", loaded.Loaded, loaded.Complete)
	}
	if loaded.Layout != "directory" {
		t.Errorf("layout = %q, want directory", loaded.Layout)
	}
	// The whole world, not the `.db2` alone: the chunks hold most of it.
	if want := int64(4096 + 113 + 65 + 4 + 8192); loaded.Bytes != want {
		t.Errorf("bytes = %d, want %d — the chunk files are part of the world", loaded.Bytes, want)
	}
	// The game's rolling save is a sibling directory named after the world. It is a world in
	// its own right and must never be mistaken for this one (03 §4.1 rule 5).
	if rolling, ok := byName["World_backup_auto-20260913-204651"]; !ok || rolling.Loaded {
		t.Errorf("the rolling save read as %+v", rolling)
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
