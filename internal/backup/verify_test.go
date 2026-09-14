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

// A 1.0 world, in the shape measured on build 25253791: a directory named what `-world`
// names, holding the generation's `.db2` and `.fwl2` beside a chunk index, an `.ok` marker and
// the chunk files (evidence/world-format-1.0-2026-09-14.md). Verification refused every one of
// these before ADR-179, because it looked for a `.db`/`.fwl` pair that 1.0 does not write —
// which made backups impossible on the current game.
func oneZeroWorld() map[string]string {
	return map[string]string{
		"worlds_local/Worild1/_main.14.db2":     strings.Repeat("world data ", 500),
		"worlds_local/Worild1/_main.14.fwl2":    "fwl2 header bytes",
		"worlds_local/Worild1/_main.14.chunks":  "chunk index",
		"worlds_local/Worild1/_main.14.ok":      "\x29\x00\x00\x00",
		"worlds_local/Worild1/20_20__1_9.chunk": strings.Repeat("chunk ", 100),
		"worlds_local/adminlist.txt":            "// List admin players ID  ONE per line",
		"cache/Worild1_biomedatacache.bin":      strings.Repeat("cache ", 100),
	}
}

func TestVerifyAcceptsAOneZeroWorld(t *testing.T) {
	found, err := Verify(archiveOf(t, oneZeroWorld()), "Worild1")
	if err != nil {
		t.Fatalf("Verify rejected a 1.0 world: %v", err)
	}
	if !found.Directory {
		t.Error("a 1.0 world was not recognised as the directory layout")
	}
	if found.FWLBytes != int64(len("fwl2 header bytes")) {
		t.Errorf("fwl2 read as %d bytes", found.FWLBytes)
	}
}

// The rolling saves 1.0 writes are sibling directories named after the world, and they hold a
// complete `_main.<gen>` set of their own. They must not stand in for the world, exactly as
// their pre-1.0 counterparts must not (03 §4.1 rule 5).
func TestVerifyRefusesWhenOnlyTheOneZeroRollingSavesAreThere(t *testing.T) {
	entries := map[string]string{
		"worlds_local/Worild1_backup_auto-20260913-204651/_main.10.db2":  strings.Repeat("d", 5000),
		"worlds_local/Worild1_backup_auto-20260913-204651/_main.10.fwl2": "fwl2 header bytes",
	}
	_, err := Verify(archiveOf(t, entries), "Worild1")
	if !errors.Is(err, ErrWorldMissing) {
		t.Fatalf("Verify returned %v, want ErrWorldMissing", err)
	}
	if !strings.Contains(err.Error(), "Worild1_backup_auto-20260913-204651") {
		t.Errorf("the error does not say what is there instead: %v", err)
	}
}

// Half a 1.0 world is not a world, the same rule the pair has always had.
func TestVerifyRefusesHalfAOneZeroWorld(t *testing.T) {
	half := oneZeroWorld()
	delete(half, "worlds_local/Worild1/_main.14.fwl2")

	_, err := Verify(archiveOf(t, half), "Worild1")
	if !errors.Is(err, ErrWorldMissing) {
		t.Fatalf("Verify returned %v, want ErrWorldMissing", err)
	}
	if !strings.Contains(err.Error(), "_main.<n>.fwl2") {
		t.Errorf("the error does not name the missing half in 1.0's own spelling: %v", err)
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

// A missing pair has two very different causes — a world saved under another name, and a
// worlds tree holding nothing the game would load — and the operator's next move differs for
// each. The error names what the archive does carry, because nothing else in the panel will
// (operator report, 14 Sep 2026).
func TestVerifySaysWhatTheArchiveHoldsInstead(t *testing.T) {
	underAnotherName := map[string]string{
		"worlds_local/Midgard.db":  strings.Repeat("world data ", 500),
		"worlds_local/Midgard.fwl": "fwl header bytes",
	}

	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    string
	}{
		{"a world saved under another name", underAnotherName, "it holds Midgard"},
		{
			"nothing the game would load",
			map[string]string{"adminlist.txt": "x"},
			"it holds no world at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(archiveOf(t, tc.entries), "Dedicated")
			if !errors.Is(err, ErrWorldMissing) {
				t.Fatalf("Verify returned %v, want ErrWorldMissing", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Verify said %q, want it to carry %q", err, tc.want)
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
