package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveOf writes a gzipped tar of exactly the named entries, including shapes Archive
// would never produce but a damaged disk can.
func archiveOf(t *testing.T, entries map[string]string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "world.tar.gz")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o664, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := closer(); err != nil {
			t.Fatal(err)
		}
	}
	return dest
}

// fixtureWorld is the world every case here is built around, matching worldsFixture's.
const fixtureWorld = "Dedicated"

// plausibleWorld is a pair that clears both size floors, laid out as the game leaves it.
func plausibleWorld() map[string]string {
	return map[string]string{
		"worlds_local/" + fixtureWorld + ".db":  strings.Repeat("world data ", 500),
		"worlds_local/" + fixtureWorld + ".fwl": "fwl header bytes",
		"adminlist.txt":                         "Steam_1\n",
	}
}

func TestVerifyAcceptsAPlausiblePair(t *testing.T) {
	got, err := Verify(archiveOf(t, plausibleWorld()), "Dedicated")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.WorldName != "Dedicated" || got.DBBytes < minDBBytes || got.FWLBytes < minFWLBytes {
		t.Fatalf("Verify reported %+v, which does not describe the archive written", got)
	}
}

// Asserts a .fwl differing from a previous backup's still verifies. B8's negative control:
// the engine rewrites that file between sessions, so noticing would reject healthy archives.
func TestVerifyIgnoresAChangedFWL(t *testing.T) {
	first := plausibleWorld()
	second := plausibleWorld()
	second["worlds_local/Dedicated.fwl"] = "entirely different header bytes"

	for _, entries := range []map[string]string{first, second} {
		if _, err := Verify(archiveOf(t, entries), "Dedicated"); err != nil {
			t.Fatalf("Verify rejected a healthy archive: %v", err)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	noFWL := plausibleWorld()
	delete(noFWL, "worlds_local/Dedicated.fwl")

	emptyDB := plausibleWorld()
	emptyDB["worlds_local/Dedicated.db"] = ""

	shortFWL := plausibleWorld()
	shortFWL["worlds_local/Dedicated.fwl"] = "abc"

	// The game's own rolling saves live beside the world and must not satisfy the pair check
	// on its behalf (03 §4.1 rule 5).
	onlyRolling := map[string]string{
		"worlds_local/Dedicated_backup_auto-20260906.db":  strings.Repeat("world data ", 500),
		"worlds_local/Dedicated_backup_auto-20260906.fwl": "fwl header bytes",
	}

	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    error
	}{
		{"no fwl", noFWL, ErrWorldMissing},
		{"no pair at all", map[string]string{"adminlist.txt": "x"}, ErrWorldMissing},
		{"only the game's rolling saves", onlyRolling, ErrWorldMissing},
		{"zero-byte db", emptyDB, ErrWorldImplausible},
		{"three-byte fwl", shortFWL, ErrWorldImplausible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(archiveOf(t, tc.entries), "Dedicated")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify returned %v, want %v", err, tc.want)
			}
		})
	}
}

// Asserts an archive whose bytes stop early reads as unreadable rather than as a world, at
// every truncation point including one that loses only gzip's trailer.
func TestVerifyRejectsATruncatedArchive(t *testing.T) {
	full := archiveOf(t, plausibleWorld())
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}

	for _, keep := range []int{0, 10, len(data) / 2, len(data) - 8} {
		cut := filepath.Join(t.TempDir(), "cut.tar.gz")
		if err := os.WriteFile(cut, data[:keep], 0o664); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(cut, "Dedicated"); !errors.Is(err, ErrArchiveUnreadable) {
			t.Fatalf("Verify of %d of %d bytes returned %v, want ErrArchiveUnreadable",
				keep, len(data), err)
		}
	}
}

func TestVerifyRejectsAMissingFile(t *testing.T) {
	_, err := Verify(filepath.Join(t.TempDir(), "absent.tar.gz"), "Dedicated")
	if !errors.Is(err, ErrArchiveUnreadable) {
		t.Fatalf("Verify of an absent file returned %v, want ErrArchiveUnreadable", err)
	}
}

// Asserts what Archive writes is what Verify accepts (02 §4.4 step 5).
func TestArchiveThenVerify(t *testing.T) {
	worlds := worldsFixture(t)
	dest := filepath.Join(t.TempDir(), "backups", "i1", "world.tar.gz")
	if _, err := Archive(worlds, dest); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := Verify(dest, "Dedicated"); err != nil {
		t.Fatalf("Verify of an archive Archive just wrote: %v", err)
	}
}
