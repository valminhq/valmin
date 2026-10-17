package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// worldsFixture lays out a stopped instance's worlds tree the way the game leaves it
// (03 §4): the pair inside worlds_local/, plus the three player lists alongside.
func worldsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "worlds_local"), 0o775); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o664); err != nil {
			t.Fatal(err)
		}
	}
	write("worlds_local/Dedicated.db", strings.Repeat("world data ", 500))
	write("worlds_local/Dedicated.fwl", "fwl header bytes")
	write("adminlist.txt", "// List admin players ID  ONE per line\nSteam_1\n")
	return root
}

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("archive is not valid gzip: %v", err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("archive is not a valid tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(body)
	}
}

// TestArchiveCapturesTheWholeWorldsTree is 03 §4.1 rule 6's pre-import snapshot: what it
// captures is what a user gets back if the import goes wrong, so the pair must be in there.
func TestArchiveCapturesTheWholeWorldsTree(t *testing.T) {
	root := worldsFixture(t)
	dest := filepath.Join(t.TempDir(), "snapshot.tar.gz")

	res, err := Archive(root, dest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 3 {
		t.Errorf("entries = %d, want 3", res.Entries)
	}

	got := readArchive(t, dest)
	for _, want := range []string{"worlds_local/Dedicated.db", "worlds_local/Dedicated.fwl", "adminlist.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("archive is missing %s; contents: %v", want, keys(got))
		}
	}
	if got["worlds_local/Dedicated.fwl"] != "fwl header bytes" {
		t.Error("archived content does not round-trip")
	}
}

// TestArchiveHashAndSizeDescribeTheFileOnDisk — the catalogue row is only as good as these.
func TestArchiveHashAndSizeDescribeTheFileOnDisk(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	res, err := Archive(worldsFixture(t), dest)
	if err != nil {
		t.Fatal(err)
	}

	onDisk, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(onDisk)) != res.SizeBytes {
		t.Errorf("size = %d, file is %d bytes", res.SizeBytes, len(onDisk))
	}
	sum := sha256.Sum256(onDisk)
	if hex.EncodeToString(sum[:]) != res.SHA256 {
		t.Error("sha256 does not match the bytes that landed — the hash was taken before the streams flushed")
	}
}

// TestArchiveLeavesNoPartFileBehind is 12 §9.4: a `.part` is deleted on recovery precisely
// because it is never a real archive, so a successful run must not leave one.
func TestArchiveLeavesNoPartFileBehind(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.tar.gz")
	if _, err := Archive(worldsFixture(t), dest); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

// TestArchiveFailsWithoutPublishingWhenTheSourceIsGone: nothing is published on failure, so
// no catalogue row can ever point at a half-written file.
func TestArchiveFailsWithoutPublishingWhenTheSourceIsGone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.tar.gz")

	if _, err := Archive(filepath.Join(dir, "no-such-worlds"), dest); err == nil {
		t.Fatal("archiving a missing tree reported success")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a failed archive published a file")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("a failed archive left its .part behind")
	}
}

// TestArchiveSkipsSymlinks is the B5-shaped hole: a link inside worlds/ must not become an
// archive entry that a later restore could follow out of the tree.
func TestArchiveSkipsSymlinks(t *testing.T) {
	root := worldsFixture(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res, err := Archive(root, filepath.Join(t.TempDir(), "snapshot.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 3 {
		t.Errorf("entries = %d, want 3 — the symlink was archived", res.Entries)
	}
}

func TestNameIsSafeForAFilename(t *testing.T) {
	got := Name("my server/../../etc", "20260831T090000Z", "01a075c5-cb8c-7dd4-a82e-fc7c0291f1c0")
	if strings.ContainsAny(got, "/\\ ") {
		t.Errorf("Name = %q, want no path or space characters", got)
	}
	if !strings.HasSuffix(got, ".tar.gz") {
		t.Errorf("Name = %q, want a .tar.gz suffix", got)
	}
}

// Asserts two archives of one instance in the same second get different filenames. The stamp
// has one-second resolution, so without the row id the catalogue would hold two rows pointing
// at one file.
func TestNameIsUniquePerCatalogueRow(t *testing.T) {
	stamp := "20260831T090000Z"
	first := Name("srv", stamp, "01a075c5-cb8c-7dd4-a82e-fc7c0291f1c0")
	second := Name("srv", stamp, "01a075c5-cb8d-76e8-86f8-86c11ccab912")
	if first == second {
		t.Errorf("two archives in the same second share the filename %q", first)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestArchiveLeavesTheBiomeCacheOut is 03 §4's split between a world and what the game derives
// from one.
//
// `<world>_biomedatacache.bin` is regenerated on the next boot — measured 14 Sep 2026 by
// deleting it and starting the server, which wrote a fresh one
// (evidence/stop-timeout-2026-09-14.md) — and on the operator's own host it was 21 MB against a
// 794 KB world, so an archive that carried it was 91% derived data.
func TestArchiveLeavesTheBiomeCacheOut(t *testing.T) {
	root := worldsFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "cache", "Dedicated_biomedatacache.bin"),
		[]byte(strings.Repeat("derived ", 4000)), 0o664,
	); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "a.tar.gz")
	if _, err := Archive(root, dest); err != nil {
		t.Fatal(err)
	}
	entries := readArchive(t, dest)

	for name := range entries {
		if strings.HasPrefix(name, "cache/") {
			t.Errorf("archive carries %q: the cache is derived, not world data", name)
		}
	}
	// The things that must survive the exclusion: the world itself and 03 §4's player lists.
	for _, want := range []string{"worlds_local/Dedicated.db", "worlds_local/Dedicated.fwl", "adminlist.txt"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("archive is missing %q", want)
		}
	}
}

// TestArchiveLeavesTheGamesOwnBackupsOut asserts the game's rolling saves stay out of an
// archive in both layouts, while the world beside them is kept.
func TestArchiveLeavesTheGamesOwnBackupsOut(t *testing.T) {
	root := worldsFixture(t)
	auto := filepath.Join(root, "worlds_local", "Dedicated_backup_auto-20260914-081726")
	if err := os.MkdirAll(auto, 0o775); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(auto, "_main.14.db2"),
		filepath.Join(root, "worlds_local", "Dedicated_backup_auto-20260903073824.db"),
		filepath.Join(root, "worlds_local", "Dedicated_backup_auto-20260903073824.fwl"),
		filepath.Join(root, "worlds_local", "Dedicated_backup_auto-20260903073824.db.old"),
	} {
		if err := os.WriteFile(path, []byte("older world"), 0o664); err != nil {
			t.Fatal(err)
		}
	}

	dest := filepath.Join(t.TempDir(), "a.tar.gz")
	if _, err := Archive(root, dest); err != nil {
		t.Fatal(err)
	}
	entries := readArchive(t, dest)

	for name := range entries {
		if strings.Contains(name, "_backup_auto-") {
			t.Errorf("archive carries the game's own backup %q", name)
		}
	}
	for _, want := range []string{"worlds_local/Dedicated.db", "worlds_local/Dedicated.fwl"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("archive is missing %q", want)
		}
	}
}

// TestIsAutoSave asserts which names are the game's rolling saves.
func TestIsAutoSave(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Dedicated_backup_auto-20260913-204651", true},
		{"Dedicated_backup_auto-20260903073824", true},
		{"Dedicated_backup_auto-20260903073824.db", true},
		{"Dedicated_backup_auto-20260903073824.fwl", true},
		{"Dedicated_backup_auto-20260903073824.db.old", true},
		{"Dedicated", false},
		{"Dedicated.db", false},
		{"Dedicated.db.old", false},
		{"Dedicated_backup_auto-", false},
		{"Dedicated_backup_auto-notes", false},
	}
	for _, tt := range tests {
		if got := IsAutoSave(tt.name); got != tt.want {
			t.Errorf("IsAutoSave(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
