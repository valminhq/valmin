package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// treeHash is the whole-tree fingerprint the byte-identical criteria are stated in: every
// path under root, with the sha256 of its contents.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		lines = append(lines, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// applyFixture stages a package, diffs it against an existing server tree, and returns
// everything Apply and Rollback need plus the pre-apply fingerprint.
func applyFixture(t *testing.T, staged, existing map[string]string) (
	changes []Change, manifest []ManifestEntry, serverRoot, backupDir, before string,
) {
	t.Helper()
	changes, serverRoot = diffFor(t, "Ns-Thing", staged, existing)
	manifest, err := Manifest(changes)
	if err != nil {
		t.Fatal(err)
	}
	backupDir = filepath.Join(t.TempDir(), "backup")
	return changes, manifest, serverRoot, backupDir, treeHash(t, serverRoot)
}

// backupAndApply is the order the install job uses: save what would be displaced across
// the whole closure, then move files.
func backupAndApply(changes []Change, serverRoot, backupDir string) error {
	if err := Backup(changes, serverRoot, backupDir); err != nil {
		return err
	}
	return Apply(changes, serverRoot)
}

func TestApplyPlacesEveryChange(t *testing.T) {
	changes, manifest, serverRoot, backupDir, _ := applyFixture(t,
		map[string]string{"Thing.dll": "payload", "assets/data.bin": "assets"}, nil)

	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, e := range manifest {
		body, err := os.ReadFile(filepath.Join(serverRoot, filepath.FromSlash(e.Path)))
		if err != nil {
			t.Fatalf("%s: %v", e.Path, err)
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != e.SHA256 {
			t.Errorf("%s: on-disk sha256 = %s, manifest says %s", e.Path, got, e.SHA256)
		}
	}
}

// TestRollbackRestoresAnOverwrittenFile is the criterion the whole ordering exists for: a
// failed install returns server/ byte-identical, which for an overwrite means the previous
// bytes come back, not that the path is deleted.
func TestRollbackRestoresAnOverwrittenFile(t *testing.T) {
	changes, manifest, serverRoot, backupDir, before := applyFixture(t,
		map[string]string{"Thing.dll": "the new version", "New.dll": "brand new"},
		map[string]string{"BepInEx/plugins/Ns-Thing/Thing.dll": "the version already there"})

	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if treeHash(t, serverRoot) == before {
		t.Fatal("Apply changed nothing; the rollback assertion below would pass for free")
	}
	if err := Rollback(manifest, serverRoot, backupDir); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := treeHash(t, serverRoot); got != before {
		t.Errorf("server tree after rollback:\n%s\nwant:\n%s", got, before)
	}
}

// TestRollbackAfterAPartialApply is the SIGKILL shape: the process stops part-way through
// the move, so only some of the manifest is on disk. Rollback still returns the tree
// exactly, because the backups were all taken before the first file moved.
func TestRollbackAfterAPartialApply(t *testing.T) {
	changes, manifest, serverRoot, backupDir, before := applyFixture(t,
		map[string]string{"A.dll": "new a", "B.dll": "new b", "C.dll": "new c"},
		map[string]string{
			"BepInEx/plugins/Ns-Thing/A.dll": "old a",
			"BepInEx/plugins/Ns-Thing/B.dll": "old b",
		})

	// Apply everything, then undo only the tail by hand — the state a process killed
	// between two files leaves behind.
	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := os.Remove(filepath.Join(serverRoot, "BepInEx", "plugins", "Ns-Thing", "C.dll")); err != nil {
		t.Fatal(err)
	}

	if err := Rollback(manifest, serverRoot, backupDir); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := treeHash(t, serverRoot); got != before {
		t.Errorf("server tree after rollback:\n%s\nwant:\n%s", got, before)
	}
}

// TestApplyBacksUpEverythingBeforeMovingAnything is the ordering itself, asserted rather
// than trusted. Interleaved — back up file N, write file N, back up N+1 — a crash after
// writing N leaves N+1 modified with no backup, and rollback would read that as a create
// and delete a file that was there before the install.
func TestApplyBacksUpEverythingBeforeMovingAnything(t *testing.T) {
	changes, _, serverRoot, backupDir, _ := applyFixture(t,
		map[string]string{"A.dll": "new a", "B.dll": "new b"},
		map[string]string{
			"BepInEx/plugins/Ns-Thing/A.dll": "old a",
			"BepInEx/plugins/Ns-Thing/B.dll": "old b",
		})
	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for name, want := range map[string]string{"A.dll": "old a", "B.dll": "old b"} {
		body, err := os.ReadFile(filepath.Join(backupDir, "BepInEx", "plugins", "Ns-Thing", name))
		if err != nil {
			t.Fatalf("no backup of %s: %v", name, err)
		}
		if string(body) != want {
			t.Errorf("backup of %s = %q, want %q", name, body, want)
		}
	}
}

// TestRollbackRefusesAManifestPathOutsideTheServerRoot is B5 on the recovery path.
// Manifest paths come out of a database column and this is a privileged process deleting
// files, so they are re-validated rather than trusted.
func TestRollbackRefusesAManifestPathOutsideTheServerRoot(t *testing.T) {
	serverRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep-me")
	if err := os.WriteFile(outside, []byte("not the panel's"), 0o600); err != nil {
		t.Fatal(err)
	}

	rel, err := filepath.Rel(serverRoot, outside)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []ManifestEntry{{Path: filepath.ToSlash(rel)}}
	if err := Rollback(manifest, serverRoot, t.TempDir()); err == nil {
		t.Error("Rollback = nil, want a refusal for a path outside the server root")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("rollback removed a file outside the server root: %v", err)
	}
}

// TestRollbackContinuesPastAFailure: stopping at the first error would leave the rest of a
// half-applied install on disk with nothing left that knows about it.
func TestRollbackContinuesPastAFailure(t *testing.T) {
	changes, manifest, serverRoot, backupDir, _ := applyFixture(t,
		map[string]string{"A.dll": "a", "B.dll": "b"}, nil)
	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	manifest = append([]ManifestEntry{{Path: "../escape"}}, manifest...)
	if err := Rollback(manifest, serverRoot, backupDir); err == nil {
		t.Fatal("Rollback = nil, want the bad entry reported")
	}
	for _, name := range []string{"A.dll", "B.dll"} {
		p := filepath.Join(serverRoot, "BepInEx", "plugins", "Ns-Thing", name)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a rollback that hit an earlier error: %v", name, err)
		}
	}
}

func TestApplySkipsWhatDiffSkipped(t *testing.T) {
	const edited = "the operator's own settings"
	changes, _, serverRoot, backupDir, before := applyFixture(t,
		map[string]string{"config/thing.cfg": "a shipped default"},
		map[string]string{"BepInEx/config/thing.cfg": edited})

	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := treeHash(t, serverRoot); got != before {
		t.Errorf("applying a skip-only plan changed the tree:\n%s\nwant:\n%s", got, before)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "BepInEx", "config", "thing.cfg")); !os.IsNotExist(err) {
		t.Error("a skipped change was backed up; nothing was going to displace it")
	}
}

func TestApplyWritesFilesWithTheInstallersOwnMode(t *testing.T) {
	changes, _, serverRoot, backupDir, _ := applyFixture(t, map[string]string{"Thing.dll": "x"}, nil)
	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	info, err := os.Stat(filepath.Join(serverRoot, "BepInEx", "plugins", "Ns-Thing", "Thing.dll"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o664 {
		t.Errorf("mode = %o, want 664 (08 §2.1, never the source's)", got)
	}
}

// TestRemoveTakesOnlyWhatTheManifestNames is B9 as a unit: uninstall deletes the recorded
// paths and nothing adjacent, and a path that is already gone is not an error — a manifest
// written before the files moved names what an install *meant* to place.
func TestRemoveTakesOnlyWhatTheManifestNames(t *testing.T) {
	changes, manifest, serverRoot, backupDir, before := applyFixture(t,
		map[string]string{"Thing.dll": "payload", "assets/data.bin": "assets"},
		map[string]string{"valheim_server.x86_64": "the game"})
	if err := backupAndApply(changes, serverRoot, backupDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// A file in the package's own directory that the manifest does not name: a heuristic
	// re-run would take the directory, and B9 forbids exactly that.
	neighbour := filepath.Join(serverRoot, "BepInEx", "plugins", "Ns-Thing", "Notes.txt")
	if err := os.WriteFile(neighbour, []byte("the admin's"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := append(Paths(manifest), "BepInEx/plugins/Ns-Thing/NeverWritten.dll")
	if err := Remove(paths, serverRoot); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("a file the manifest does not name was removed: %v", err)
	}
	if err := os.Remove(neighbour); err != nil {
		t.Fatal(err)
	}
	if got := treeHash(t, serverRoot); got != before {
		t.Errorf("tree after remove:\n%s\nwant:\n%s", got, before)
	}
}

// TestRemoveRefusesAPathOutsideTheServerRoot. The paths come out of a database column and
// this process removes files as the panel, so they are re-validated here rather than
// trusted — the same reason Rollback re-validates them.
func TestRemoveRefusesAPathOutsideTheServerRoot(t *testing.T) {
	serverRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(outside, []byte("not the panel's"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../keep.txt", "/etc/passwd", "a/../../keep.txt", ""} {
		if err := Remove([]string{p}, serverRoot); !errors.Is(err, ErrUnsafeDest) {
			t.Errorf("Remove(%q) = %v, want ErrUnsafeDest", p, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the server root was removed: %v", err)
	}
}

// TestRollbackClearsAnInterruptedWrite is the regression for what WP-M2-12's AT-M2-4 found
// against the real binary: a panel killed between CreateTemp and rename leaves a
// `.valmin-XXXX` file beside the destination, no manifest names it, and a rollback that
// walked manifest paths alone left it there — so `server/` came back *nearly* identical.
//
// `↯` This is the *only* deterministic guard on it, and that is the point of writing it.
// AT-M2-4 found the defect on its first run and does **not** reproduce it every run: whether
// a temp file exists at all depends on where in the rename loop the SIGKILL lands, and the
// same test passed with this fix removed on a later attempt. The acceptance test proves the
// rollback; this one proves what the rollback has to include.
func TestRollbackClearsAnInterruptedWrite(t *testing.T) {
	root := t.TempDir()
	backup := t.TempDir()

	dir := filepath.Join(root, "BepInEx", "plugins", "Ns-Thing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What the manifest names, already placed.
	if err := os.WriteFile(filepath.Join(dir, "Thing.dll"), []byte("placed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And what a killed copyFile left behind next to it.
	stale := filepath.Join(dir, tempPrefix+"391827")
	if err := os.WriteFile(stale, []byte("half a file"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := []ManifestEntry{{Path: "BepInEx/plugins/Ns-Thing/Thing.dll"}}
	if err := Rollback(manifest, root, backup); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the interrupted write survived the rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Thing.dll")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the manifested file survived the rollback")
	}
}

// TestRollbackLeavesFilesItDoesNotOwn is the other half, and the reason the sweep is scoped
// to a prefix rather than to "anything unexpected": an operator's own dotfile in a directory
// a mod happens to write into is not the panel's to delete.
func TestRollbackLeavesFilesItDoesNotOwn(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "BepInEx", "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(keep, []byte("*.bak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Thing.cfg"), []byte("shipped"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := []ManifestEntry{{Path: "BepInEx/config/Thing.cfg"}}
	if err := Rollback(manifest, root, t.TempDir()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("rollback removed a file no manifest named and no job wrote: %v", err)
	}
}
