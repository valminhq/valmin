package control

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

// cacheFixture is a data root holding builds, zips and setup blobs, some used and some not.
type cacheFixture struct {
	builds, zips, blobs string
	usedBlob, zipBlob   string
	keptBlob            string
}

// writeCacheFile creates path with content, making its directory first.
func writeCacheFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// digestOf is the setup blob name of content.
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// seedCaches fills w's data root. inst-a runs build 100 with Used-Mod installed; a saved setup
// names build 200, Setup-Mod and one config blob. Orphan-Mod is unused and also linked as an
// orphan blob, Gone-Mod is unused, and an orphan blob links Used-Mod's zip.
func seedCaches(t *testing.T, w *world) cacheFixture {
	t.Helper()
	root := w.cfg.Data.Root
	f := cacheFixture{
		builds: instance.CacheDir(root),
		zips:   cache.RootFor(root, source.Thunderstore),
		blobs:  setupblob.Dir(root),
	}
	seedInstance(t, w, string(instance.StateStopped))
	seed(t, w.db, `UPDATE instances SET game_build_id = '100' WHERE id = 'inst-a'`)
	seed(t, w.db, `INSERT INTO instance_mods (
		instance_id, full_name, source, version, installed_as, file_manifest, installed_at
	) VALUES ('inst-a', 'Used-Mod', 'thunderstore', '1.0.0', 'explicit', '[]', ?)`, store.Now())
	if err := w.db.CreateBackup(t.Context(), &store.Backup{
		ID: "b1", InstanceID: "inst-a", Path: "/tmp/b1", SHA256: "hash",
		WorldName: "World", Trigger: store.TriggerManual, Consistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	f.keptBlob = digestOf("config")
	if err := w.db.SaveSetup(t.Context(), &store.SavedSetup{
		ID: "s1", InstanceID: "inst-a", Name: "s1", GameBuildID: "200", WorldName: "World",
		SnapshotJSON: `{"mods":[{"full_name":"Setup-Mod","source":"thunderstore","version":"1.0.0"}]}`,
		BackupID:     "b1",
	}, []store.SetupArtifactRef{{
		FullName: "Setup-Mod", Source: "thunderstore", Version: "1.0.0", Kind: "files", SHA256: f.keptBlob,
	}}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"100", "200"} {
		writeCacheFile(t, filepath.Join(f.builds, id, "valheim_server.x86_64"), "game "+id)
	}
	for _, name := range []string{"Used-Mod-1.0.0", "Setup-Mod-1.0.0", "Orphan-Mod-1.0.0", "Gone-Mod-1.0.0"} {
		writeCacheFile(t, filepath.Join(f.zips, name+".zip"), "zip "+name)
	}
	writeCacheFile(t, filepath.Join(f.blobs, f.keptBlob), "config")
	f.zipBlob, f.usedBlob = digestOf("orphan"), digestOf("used")
	for blob, zip := range map[string]string{f.zipBlob: "Orphan-Mod-1.0.0", f.usedBlob: "Used-Mod-1.0.0"} {
		if err := os.Link(filepath.Join(f.zips, zip+".zip"), filepath.Join(f.blobs, blob)); err != nil {
			t.Fatal(err)
		}
	}
	// Zips and builds are judged by their mtime, which is set back past the grace period. Setup
	// files are judged by their ctime, which cannot be, so the clock moves to just past the
	// grace period of everything written so far, and no further: a ctime the prune itself moves
	// stays inside it.
	old := time.Now().Add(-2 * CacheGrace)
	for _, path := range []string{
		filepath.Join(f.builds, "100"), filepath.Join(f.builds, "200"),
		filepath.Join(f.zips, "Used-Mod-1.0.0.zip"), filepath.Join(f.zips, "Setup-Mod-1.0.0.zip"),
		filepath.Join(f.zips, "Orphan-Mod-1.0.0.zip"), filepath.Join(f.zips, "Gone-Mod-1.0.0.zip"),
	} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	seeded := time.Now()
	w.c.CachePruner.Now = func() time.Time { return seeded.Add(CacheGrace + time.Nanosecond) }
	time.Sleep(20 * time.Millisecond)
	return f
}

func TestCacheInventoryCountsSharedBytesOnceAndOnlyWhenAllNamesGo(t *testing.T) {
	w := newWorld(t)
	f := seedCaches(t, w)

	entries, err := w.c.CachePruner.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CacheEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	for name, want := range map[string]struct {
		inUse       bool
		reclaimable bool
	}{
		"100": {true, false}, "200": {false, true},
		"Used-Mod-1.0.0": {true, false}, "Setup-Mod-1.0.0": {true, false}, "Gone-Mod-1.0.0": {false, true},
		f.keptBlob: {true, false},
		// Used-Mod's zip keeps the bytes, so its orphan blob frees nothing.
		f.usedBlob: {false, false},
	} {
		e, ok := byName[name]
		if !ok {
			t.Errorf("%s missing from the inventory", name)
			continue
		}
		if e.InUse() != want.inUse || (e.ReclaimableBytes > 0) != want.reclaimable {
			t.Errorf("%s: in use %v reclaimable %d, want in use %v reclaimable %v",
				name, e.InUse(), e.ReclaimableBytes, want.inUse, want.reclaimable)
		}
	}
	// Orphan-Mod's zip and blob are one inode: its size counts once between them.
	orphanZip, orphanBlob := byName["Orphan-Mod-1.0.0"], byName[f.zipBlob]
	if got := orphanZip.ReclaimableBytes + orphanBlob.ReclaimableBytes; got != orphanZip.SizeBytes {
		t.Errorf("orphan zip and blob reclaim %d bytes together, want %d once", got, orphanZip.SizeBytes)
	}
	if byName["Setup-Mod-1.0.0"].Setups != 1 || len(byName["100"].Servers) != 1 {
		t.Errorf("users not reported: setup mod %+v, build 100 %+v", byName["Setup-Mod-1.0.0"], byName["100"])
	}
}

func TestCachePruneRemovesOnlyWhatNothingUses(t *testing.T) {
	w := newWorld(t)
	f := seedCaches(t, w)

	job, err := w.c.CachePruner.Submit(t.Context(), &jobs.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if got := awaitJob(t, w, job.ID); got.Status != jobs.StatusSucceeded {
		t.Fatalf("prune job = %s %v, want succeeded", got.Status, got.Error)
	}
	for path, kept := range map[string]bool{
		filepath.Join(f.builds, "100"):                true,
		filepath.Join(f.builds, "200"):                false,
		filepath.Join(f.zips, "Used-Mod-1.0.0.zip"):   true,
		filepath.Join(f.zips, "Setup-Mod-1.0.0.zip"):  true,
		filepath.Join(f.zips, "Orphan-Mod-1.0.0.zip"): false,
		filepath.Join(f.zips, "Gone-Mod-1.0.0.zip"):   false,
		filepath.Join(f.blobs, f.keptBlob):            true,
		filepath.Join(f.blobs, f.zipBlob):             false,
		filepath.Join(f.blobs, f.usedBlob):            false,
		filepath.Join(f.builds, "200.trash"):          false,
	} {
		_, err := os.Stat(path)
		if exists := err == nil; exists != kept {
			t.Errorf("%s exists = %v after prune, want %v", path, exists, kept)
		}
	}
}

func TestCachePruneSparesWhatChangedWithinTheGracePeriod(t *testing.T) {
	w := newWorld(t)
	f := seedCaches(t, w)
	w.c.CachePruner.Now = time.Now
	now := time.Now()
	for _, path := range []string{filepath.Join(f.builds, "200"), filepath.Join(f.zips, "Gone-Mod-1.0.0.zip")} {
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}

	job, err := w.c.CachePruner.Submit(t.Context(), &jobs.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if got := awaitJob(t, w, job.ID); got.Status != jobs.StatusSucceeded {
		t.Fatalf("prune job = %s %v, want succeeded", got.Status, got.Error)
	}
	for _, path := range []string{
		filepath.Join(f.builds, "200"), filepath.Join(f.zips, "Gone-Mod-1.0.0.zip"), filepath.Join(f.blobs, f.zipBlob),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was pruned inside its grace period: %v", path, err)
		}
	}
}
