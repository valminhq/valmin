package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// stage builds a staging directory from a path -> contents map, the shape
// internal/mods/extract leaves behind. Paths are slash-separated regardless of platform.
func stage(t *testing.T, files map[string]string) string {
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

func dests(t *testing.T, stagingDir, fullName string) []string {
	t.Helper()
	placements, err := Plan(stagingDir, fullName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out := make([]string, 0, len(placements))
	for _, p := range placements {
		out = append(out, p.Dest)
	}
	return out
}

// metadataFiles is the four names 03 §6.4 excludes, present in every fixture below so no
// test accidentally passes because its fixture had none.
func metadataFiles(files map[string]string) map[string]string {
	files["manifest.json"] = `{"name":"x"}`
	files["icon.png"] = "\x89PNG"
	files["README.md"] = "readme"
	files["CHANGELOG.md"] = "changelog"
	return files
}

// TestPlanMergesAFrameworkWrapperIntoServerRoot is 03 §6.4's heuristic 1 against
// denikson-BepInExPack_Valheim's real layout: the wrapper directory's contents land at the
// server root and the wrapper name itself appears in no destination.
func TestPlanMergesAFrameworkWrapperIntoServerRoot(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{
		"BepInExPack_Valheim/BepInEx/core/BepInEx.Preloader.dll": "preloader",
		"BepInExPack_Valheim/BepInEx/config/BepInEx.cfg":         "[Logging.Console]\nEnabled = true\n",
		"BepInExPack_Valheim/doorstop_libs/libdoorstop_x64.so":   "so",
		"BepInExPack_Valheim/doorstop_config.ini":                "ini",
		"BepInExPack_Valheim/winhttp.dll":                        "dll",
	}))

	want := []string{
		"BepInEx/config/BepInEx.cfg",
		"BepInEx/core/BepInEx.Preloader.dll",
		"doorstop_config.ini",
		"doorstop_libs/libdoorstop_x64.so",
		"winhttp.dll",
	}
	if got := dests(t, dir, "denikson-BepInExPack_Valheim"); !reflect.DeepEqual(got, want) {
		t.Errorf("dests = %q, want %q", got, want)
	}
}

// TestPlanTreatsALoneDirectoryWithoutBepInExAsAPlugin is frameworkWrapper's negative
// control. A plugin that ships its files inside one folder has heuristic 1's shape and is
// not a framework pack; without the BepInEx/ check it would be merged into the server root
// beside the game binary.
func TestPlanTreatsALoneDirectoryWithoutBepInExAsAPlugin(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{
		"MyMod/MyMod.dll":        "dll",
		"MyMod/assets/bundle.ab": "assets",
	}))

	want := []string{
		"BepInEx/plugins/Some-MyMod/MyMod/MyMod.dll",
		"BepInEx/plugins/Some-MyMod/MyMod/assets/bundle.ab",
	}
	if got := dests(t, dir, "Some-MyMod"); !reflect.DeepEqual(got, want) {
		t.Errorf("dests = %q, want %q", got, want)
	}
}

// TestPlanMergesTopLevelBepInExTrees is heuristic 2 and 3: a top-level BepInEx/ merges at
// the server root, and plugins/, config/ and patchers/ merge one level inside it.
func TestPlanMergesTopLevelBepInExTrees(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "BepInEx tree merges at the server root",
			files: map[string]string{"BepInEx/plugins/Thing.dll": "dll"},
			want:  []string{"BepInEx/plugins/Thing.dll"},
		},
		{
			// ValheimModding-Jotunn's real layout.
			name:  "plugins merges into BepInEx",
			files: map[string]string{"plugins/Jotunn.dll": "dll", "plugins/Jotunn.xml": "xml"},
			want:  []string{"BepInEx/plugins/Jotunn.dll", "BepInEx/plugins/Jotunn.xml"},
		},
		{
			name:  "patchers merges into BepInEx",
			files: map[string]string{"patchers/Patch.dll": "dll"},
			want:  []string{"BepInEx/patchers/Patch.dll"},
		},
		{
			name:  "config merges into BepInEx",
			files: map[string]string{"config/thing.yml": "yml"},
			want:  []string{"BepInEx/config/thing.yml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := stage(t, metadataFiles(tt.files))
			if got := dests(t, dir, "Ns-Name"); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dests = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPlanNamespacesLooseFiles is heuristic 4, Smoothbrain-Sailing's real layout.
func TestPlanNamespacesLooseFiles(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{"Sailing.dll": "dll"}))

	want := []string{"BepInEx/plugins/Smoothbrain-Sailing/Sailing.dll"}
	if got := dests(t, dir, "Smoothbrain-Sailing"); !reflect.DeepEqual(got, want) {
		t.Errorf("dests = %q, want %q", got, want)
	}
}

// TestPlanClassifiesEachTopLevelEntryIndependently is F1, the finding ADR-106 exists for
// and the regression that would return if classification went back to first-match-wins.
// Therzie-Warfare ships a root .dll *and* a top-level config/: under 03 §6.4's original
// "apply in order" the config/ match ended classification and Warfare.dll — the entire
// mod — was never placed.
func TestPlanClassifiesEachTopLevelEntryIndependently(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{
		"Warfare.dll": "dll",
		"config/TherzieTranslations/Warfare/Warfare.English.yml": "yml",
	}))

	want := []string{
		"BepInEx/plugins/Therzie-Warfare/Warfare.dll",
		"BepInEx/config/TherzieTranslations/Warfare/Warfare.English.yml",
	}
	got := dests(t, dir, "Therzie-Warfare")
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("dests = %q, missing %q", got, w)
		}
	}
}

func TestPlanExcludesPackageMetadata(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{"Thing.dll": "dll", "readme.MD": "lowercased"}))

	want := []string{"BepInEx/plugins/Ns-Thing/Thing.dll"}
	if got := dests(t, dir, "Ns-Thing"); !reflect.DeepEqual(got, want) {
		t.Errorf("dests = %q, want %q — metadata is excluded case-insensitively", got, want)
	}
}

// TestPlanKeepsMetadataNamesInsideThePayload: the exclusion is a top-level packaging
// convention, so a mod that legitimately ships its own README.md next to its DLL keeps it.
func TestPlanKeepsMetadataNamesInsideThePayload(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{
		"docs/README.md": "the mod's own docs",
		"Thing.dll":      "dll",
	}))

	if got := dests(t, dir, "Ns-Thing"); !slices.Contains(got, "BepInEx/plugins/Ns-Thing/docs/README.md") {
		t.Errorf("dests = %q, want the payload's own README.md kept", got)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{
		"a.dll": "a", "b.dll": "b", "config/z.yml": "z", "config/a.yml": "a",
		"plugins/nested/deep.dll": "d", "plugins/top.dll": "t",
	}))

	first, err := Plan(dir, "Ns-Name")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := Plan(dir, "Ns-Name")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("Plan is not deterministic:\n%+v\n%+v", first, again)
		}
	}
}

// TestPlanEmptyStagingDirectory: a package with nothing but metadata plans nothing, and
// that is not an error — WP-07 is where an empty plan becomes package_invalid, with the
// manifest to say so.
func TestPlanEmptyStagingDirectory(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{}))
	placements, err := Plan(dir, "Ns-Name")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(placements) != 0 {
		t.Errorf("placements = %+v, want none", placements)
	}
}

// TestPlanRefusesATraversingFullName is B5. full_name comes from the Thunderstore listing,
// and path.Join resolves `..` while building the namespaced plugin directory — so without
// this guard a package named "../../../../etc/cron.d" plans destinations outside the server
// root, records them in the manifest, and WP-07 writes them as a privileged process.
func TestPlanRefusesATraversingFullName(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{"Evil.dll": "payload"}))

	for _, fullName := range []string{
		"../../../../etc/cron.d",
		"..",
		"Ns/../../..",
		`Ns\..\..`,
		"",
	} {
		t.Run(fullName, func(t *testing.T) {
			placements, err := Plan(dir, fullName)
			if !errors.Is(err, ErrInvalidFullName) {
				t.Fatalf("Plan(%q) = %+v, %v; want ErrInvalidFullName", fullName, placements, err)
			}
		})
	}
}

// TestPlanRefusesTwoEntriesClaimingOneDestination: per-entry classification (ADR-106) lets
// a package ship both plugins/Shared.dll and BepInEx/plugins/Shared.dll, which land on one
// path. Allowing it would put that path in the manifest twice with two different hashes,
// only one of which could match disk — ADR-009's exact uninstall, broken by the package.
func TestPlanRefusesTwoEntriesClaimingOneDestination(t *testing.T) {
	dir := stage(t, metadataFiles(map[string]string{
		"plugins/Shared.dll":         "one",
		"BepInEx/plugins/Shared.dll": "the other",
	}))

	var dup *DuplicateDestError
	if _, err := Plan(dir, "Ns-Name"); !errors.As(err, &dup) {
		t.Fatalf("Plan = %v, want a *DuplicateDestError", err)
	}
	if dup.Dest != "BepInEx/plugins/Shared.dll" {
		t.Errorf("dest = %q, want the contested path named", dup.Dest)
	}
}
