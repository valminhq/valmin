package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// server builds a server root from a path -> contents map, standing in for the tree an
// instance already has on disk.
func server(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func diffFor(t *testing.T, fullName string, staged, existing map[string]string,
) (changes []Change, serverRoot string) {
	t.Helper()
	dir := stage(t, metadataFiles(staged))
	serverRoot = server(t, existing)
	placements, err := Plan(dir, fullName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	changes, err = Diff(fullName, placements, serverRoot, nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return changes, serverRoot
}

func changeFor(t *testing.T, changes []Change, dest string) Change {
	t.Helper()
	for _, c := range changes {
		if c.Dest == dest {
			return c
		}
	}
	t.Fatalf("no change for %q in %+v", dest, changes)
	return Change{}
}

func TestDiffReportsCreateAndOverwrite(t *testing.T) {
	changes, _ := diffFor(t, "Ns-Thing",
		map[string]string{"New.dll": "new", "Old.dll": "replacement"},
		map[string]string{"BepInEx/plugins/Ns-Thing/Old.dll": "the version already installed"})

	if got := changeFor(t, changes, "BepInEx/plugins/Ns-Thing/New.dll").Action; got != ActionCreate {
		t.Errorf("New.dll action = %q, want %q", got, ActionCreate)
	}
	if got := changeFor(t, changes, "BepInEx/plugins/Ns-Thing/Old.dll").Action; got != ActionOverwrite {
		t.Errorf("Old.dll action = %q, want %q", got, ActionOverwrite)
	}
}

// TestDiffNeverOverwritesAnExistingConfig is 03 §6.4's user-settings rule and B10. The
// shipped default is reported as a skip with a reason — a line in the diff, not a silence —
// and the operator's own bytes are what a later apply must leave alone.
func TestDiffNeverOverwritesAnExistingConfig(t *testing.T) {
	const edited = "[Logging.Console]\n# the operator turned this on by hand\nEnabled = true\n"

	changes, root := diffFor(t, "denikson-BepInExPack_Valheim",
		map[string]string{
			"config/BepInEx.cfg": "[Logging.Console]\nEnabled = false\n",
			"config/fresh.cfg":   "a default that is not on disk yet",
		},
		map[string]string{"BepInEx/config/BepInEx.cfg": edited})

	skipped := changeFor(t, changes, "BepInEx/config/BepInEx.cfg")
	if skipped.Action != ActionSkip {
		t.Errorf("BepInEx.cfg action = %q, want %q", skipped.Action, ActionSkip)
	}
	if skipped.Reason == "" {
		t.Error("a skip must carry a reason; a silent skip is what 03 §6.4 forbids")
	}
	if got := changeFor(t, changes, "BepInEx/config/fresh.cfg").Action; got != ActionCreate {
		t.Errorf("fresh.cfg action = %q, want %q — an absent default is still shipped", got, ActionCreate)
	}

	// Diff decides; it must not have touched the file it decided to leave alone.
	body, err := os.ReadFile(filepath.Join(root, "BepInEx", "config", "BepInEx.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != edited {
		t.Errorf("BepInEx.cfg = %q, want it byte-identical", body)
	}
}

// TestDiffSkippedConfigIsNotInTheManifest is the other half of the rule: what install did
// not write, uninstall must not delete. A user-edited .cfg surviving an uninstall depends
// entirely on it being absent here.
func TestDiffSkippedConfigIsNotInTheManifest(t *testing.T) {
	changes, _ := diffFor(t, "Ns-Thing",
		map[string]string{"config/thing.cfg": "shipped default", "Thing.dll": "dll"},
		map[string]string{"BepInEx/config/thing.cfg": "the operator's edits"})

	manifest, err := Manifest(changes)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range manifest {
		if e.Path == "BepInEx/config/thing.cfg" {
			t.Fatalf("manifest = %+v, must not name the skipped config", manifest)
		}
	}
	if len(manifest) != 1 {
		t.Errorf("manifest = %+v, want only the .dll", manifest)
	}
}

// TestDiffConflictsWithAnotherPackagesClaim is 11 §2.5's mod_conflict: overwriting a path
// another package's manifest owns would leave that path in two manifests, and the other
// package's uninstall would then delete a file it no longer wrote.
func TestDiffConflictsWithAnotherPackagesClaim(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{"plugins/Shared.dll": "mine"}))
	placements, err := Plan(dir, "Ns-Mine")
	if err != nil {
		t.Fatal(err)
	}

	claims := map[string]string{"BepInEx/plugins/Shared.dll": "Other-Package"}
	_, err = Diff("Ns-Mine", placements, server(t, nil), claims)

	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Diff error = %v, want a *ConflictError", err)
	}
	if conflict.Path != "BepInEx/plugins/Shared.dll" || conflict.Owner != "Other-Package" {
		t.Errorf("conflict = %+v, want the path and the owning package named", conflict)
	}
}

// TestDiffDoesNotConflictWithItsOwnClaim: an update reinstalls the same full_name over its
// own manifest, which is the normal path and not a conflict.
func TestDiffDoesNotConflictWithItsOwnClaim(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{"plugins/Mine.dll": "v2"}))
	placements, err := Plan(dir, "Ns-Mine")
	if err != nil {
		t.Fatal(err)
	}

	claims := map[string]string{"BepInEx/plugins/Mine.dll": "Ns-Mine"}
	if _, err := Diff("Ns-Mine", placements, server(t, nil), claims); err != nil {
		t.Fatalf("Diff = %v, want no error for a package's own path", err)
	}
}

// TestDiffRejectsADestinationThatIsADirectory: no copy can succeed onto it, so it is
// reported rather than folded into create or overwrite.
func TestDiffRejectsADestinationThatIsADirectory(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{"plugins/Thing.dll": "dll"}))
	placements, err := Plan(dir, "Ns-Thing")
	if err != nil {
		t.Fatal(err)
	}

	root := server(t, map[string]string{"BepInEx/plugins/Thing.dll/nested": "surprise"})
	if _, err := Diff("Ns-Thing", placements, root, nil); err == nil {
		t.Error("Diff = nil, want an error for a destination that exists as a directory")
	}
}

// TestManifestHashesTheBytesThatWillBeWritten is ADR-009: the manifest is only exact if
// its hash is the hash of the staged bytes the applier copies.
func TestManifestHashesTheBytesThatWillBeWritten(t *testing.T) {
	const body = "the payload bytes"
	changes, _ := diffFor(t, "Ns-Thing", map[string]string{"Thing.dll": body}, nil)

	manifest, err := Manifest(changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 {
		t.Fatalf("manifest = %+v, want one entry", manifest)
	}
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); manifest[0].SHA256 != want {
		t.Errorf("sha256 = %q, want %q", manifest[0].SHA256, want)
	}
	if manifest[0].Path != "BepInEx/plugins/Ns-Thing/Thing.dll" {
		t.Errorf("path = %q, want the destination rather than the staging path", manifest[0].Path)
	}
}

// TestDiffRefusesADestinationOutsideTheServerRoot is B5 at the second entry point. Plan
// cannot produce such a placement, but Diff is the step that joins a destination onto the
// server root and hands it to the applier, so it refuses one rather than assuming its
// caller used Plan.
func TestDiffRefusesADestinationOutsideTheServerRoot(t *testing.T) {
	for _, dest := range []string{"../../etc/cron.d/evil", "/etc/cron.d/evil", "..", ""} {
		t.Run(dest, func(t *testing.T) {
			placements := []Placement{{Source: "/staging/evil", Dest: dest}}
			if _, err := Diff("Ns-Evil", placements, server(t, nil), nil); !errors.Is(err, ErrUnsafeDest) {
				t.Fatalf("Diff(%q) = %v, want ErrUnsafeDest", dest, err)
			}
		})
	}
}
