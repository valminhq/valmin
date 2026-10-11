package installer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeAt(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readAt reports a file's body, or "" when there is none.
func readAt(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestParkAndUnparkRoundTrip is Q37's promise: disabling moves the loadable files out and
// leaves the admin's config, and enabling puts the same bytes back at the same paths.
func TestParkAndUnparkRoundTrip(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	writeAt(t, server, "BepInEx/plugins/Mod/Mod.dll", "code")
	writeAt(t, server, "BepInEx/config/Mod.cfg", "mine")
	manifest := []ManifestEntry{
		{Path: "BepInEx/plugins/Mod/Mod.dll"},
		{Path: "BepInEx/config/Mod.cfg"},
		{Path: "BepInEx/plugins/Mod/Deleted.dll"},
	}

	movable := Movable(manifest)
	if len(movable) != 2 {
		t.Fatalf("movable = %v, want the two plugin paths and not the config", movable)
	}
	moved, err := Park(movable, server, parking)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0] != "BepInEx/plugins/Mod/Mod.dll" {
		t.Fatalf("moved = %v, want only the file that existed", moved)
	}
	if readAt(t, server, "BepInEx/plugins/Mod/Mod.dll") != "" {
		t.Error("the plugin is still where BepInEx would load it")
	}
	if readAt(t, parking, "BepInEx/plugins/Mod/Mod.dll") != "code" {
		t.Error("the parked copy is not the plugin's bytes")
	}
	if readAt(t, server, "BepInEx/config/Mod.cfg") != "mine" {
		t.Error("the admin's config moved")
	}

	marked := MarkParked(manifest, moved)
	if got := ParkedPaths(marked); len(got) != 1 {
		t.Fatalf("parked paths = %v", got)
	}
	if err := Unpark(ParkedPaths(marked), parking, server); err != nil {
		t.Fatal(err)
	}
	if readAt(t, server, "BepInEx/plugins/Mod/Mod.dll") != "code" {
		t.Error("enabling did not put the plugin back")
	}
	if readAt(t, parking, "BepInEx/plugins/Mod/Mod.dll") != "" {
		t.Error("the parked copy outlived the enable")
	}
}

// TestUnparkRefusesToOverwriteSomethingElse asserts a file someone else put at a parked path
// is never replaced: neither copy is the panel's to discard.
func TestUnparkRefusesToOverwriteSomethingElse(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	writeAt(t, parking, "BepInEx/plugins/Mod.dll", "parked")
	writeAt(t, server, "BepInEx/plugins/Mod.dll", "someone else's")

	if err := Unpark([]string{"BepInEx/plugins/Mod.dll"}, parking, server); !errors.Is(err, ErrParkConflict) {
		t.Fatalf("err = %v, want ErrParkConflict", err)
	}
	if readAt(t, server, "BepInEx/plugins/Mod.dll") != "someone else's" {
		t.Error("the other file was overwritten")
	}
}

// TestUnparkOfAMissingCopyFails asserts enabling never succeeds with a file missing: the
// manifest says it is parked, so its absence is a fault to report.
func TestUnparkOfAMissingCopyFails(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	if err := Unpark([]string{"BepInEx/plugins/Mod.dll"}, parking, server); err == nil {
		t.Fatal("Unpark of a parked copy that does not exist succeeded")
	}
}

// TestSettleFinishesEitherInterruptedMove covers the crash sweep: a move is a copy then a
// removal, so an interruption leaves the file in both trees or only in the wrong one. Settle
// returns each file to where its manifest entry says it is.
func TestSettleFinishesEitherInterruptedMove(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	// Recorded in the server root: a disable stopped after the copy (both), and one stopped
	// after the removal of a second file whose parked copy is all that is left.
	writeAt(t, server, "BepInEx/plugins/Both.dll", "both")
	writeAt(t, parking, "BepInEx/plugins/Both.dll", "both")
	writeAt(t, parking, "BepInEx/plugins/OnlyParked.dll", "only parked")
	// Recorded as parked: an enable stopped with the file back in the server root only.
	writeAt(t, server, "BepInEx/plugins/Back.dll", "back")

	err := Settle([]ManifestEntry{
		{Path: "BepInEx/plugins/Both.dll"},
		{Path: "BepInEx/plugins/OnlyParked.dll"},
		{Path: "BepInEx/plugins/Back.dll", Parked: true},
	}, server, parking)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string][2]string{
		"BepInEx/plugins/Both.dll":       {"both", ""},
		"BepInEx/plugins/OnlyParked.dll": {"only parked", ""},
		"BepInEx/plugins/Back.dll":       {"", "back"},
	} {
		if got := readAt(t, server, rel); got != want[0] {
			t.Errorf("%s in the server root = %q, want %q", rel, got, want[0])
		}
		if got := readAt(t, parking, rel); got != want[1] {
			t.Errorf("%s in the parking tree = %q, want %q", rel, got, want[1])
		}
	}
}

// TestSettleLeavesTwoDifferentFilesAlone asserts the one case Settle refuses to decide.
func TestSettleLeavesTwoDifferentFilesAlone(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	writeAt(t, server, "BepInEx/plugins/Mod.dll", "server")
	writeAt(t, parking, "BepInEx/plugins/Mod.dll", "parked")

	err := Settle([]ManifestEntry{{Path: "BepInEx/plugins/Mod.dll", Parked: true}}, server, parking)
	if !errors.Is(err, ErrParkConflict) {
		t.Fatalf("err = %v, want ErrParkConflict", err)
	}
	if readAt(t, server, "BepInEx/plugins/Mod.dll") != "server" ||
		readAt(t, parking, "BepInEx/plugins/Mod.dll") != "parked" {
		t.Error("Settle touched one of two files it could not choose between")
	}
}

// TestSettleReportsAParkedFileNeitherTreeHolds asserts a file deleted from under a disabled
// package is reported. Park flags only what it moved, so its absence is not a settled state,
// and a Settle that called it one would leave the package unenableable with nothing said.
func TestSettleReportsAParkedFileNeitherTreeHolds(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	writeAt(t, parking, "BepInEx/plugins/Here.dll", "here")

	err := Settle([]ManifestEntry{
		{Path: "BepInEx/plugins/Gone.dll", Parked: true},
		{Path: "BepInEx/plugins/Here.dll", Parked: true},
	}, server, parking)
	if !errors.Is(err, ErrParkedFileMissing) {
		t.Fatalf("err = %v, want ErrParkedFileMissing", err)
	}
	if readAt(t, parking, "BepInEx/plugins/Here.dll") != "here" {
		t.Error("the file that was where the manifest said moved anyway")
	}
}

// TestParkRefusesAPathOutOfTheTree is B5 for the new operation: manifest paths come from a
// database column, so they are validated before anything moves.
func TestParkRefusesAPathOutOfTheTree(t *testing.T) {
	server, parking := t.TempDir(), t.TempDir()
	if _, err := Park([]string{"../outside.dll"}, server, parking); !errors.Is(err, ErrUnsafeDest) {
		t.Errorf("err = %v, want ErrUnsafeDest", err)
	}
}
