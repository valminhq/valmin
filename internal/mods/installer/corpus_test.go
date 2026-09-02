package installer

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/resolver"
)

// corpusExpectations are the placements ADR-106 must produce for the packages whose real
// layouts drove it. Every entry is a substring-free exact destination: a heuristic that
// drifts changes these, and a package the corpus happens not to contain is simply not
// asserted rather than silently passing.
var corpusExpectations = map[string][]string{
	// Heuristic 1: the wrapper's contents land at the server root.
	"denikson-BepInExPack_Valheim": {
		"BepInEx/core/BepInEx.Preloader.dll",
		"doorstop_config.ini",
		"doorstop_libs/libdoorstop_x64.so",
		"winhttp.dll",
	},
	// Heuristic 3: a top-level plugins/ merges into the shared BepInEx tree.
	"ValheimModding-Jotunn": {"BepInEx/plugins/Jotunn.dll"},
	// Heuristic 4: loose files are namespaced by package full name.
	"Smoothbrain-Sailing": {"BepInEx/plugins/Smoothbrain-Sailing/Sailing.dll"},
	// F1: a root .dll *and* a top-level config/, both placed (ADR-106).
	"Therzie-Warfare": {
		"BepInEx/plugins/Therzie-Warfare/Warfare.dll",
		"BepInEx/config/TherzieTranslations/Warfare/Warfare.English-LatestUpToDate.yml",
	},
	// F3: a Windows-built zip storing `plugins\AutoRepair.dll` as one name. extract
	// normalises the separator, so Plan sees a directory and heuristic 3 applies — the
	// alternative is a file literally called `plugins\AutoRepair.dll` under the namespaced
	// plugin directory, which BepInEx would never load.
	"Tekla-AutoRepair": {"BepInEx/plugins/AutoRepair.dll"},
}

// TestPlanOverTheRealCorpus runs extract + Plan over every downloaded package and tables
// the classification, per WP-M2-06's own risk note: five asserted packages prove the
// heuristics that exist, and the table is where a sixth layout shape would show up.
//
// The corpus is never committed (ADR-105) — mods are downloaded, not vendored — so this
// skips unless VALMIN_MOD_CORPUS names a local directory. `make test` and CI never depend
// on it.
func TestPlanOverTheRealCorpus(t *testing.T) {
	dir := os.Getenv("VALMIN_MOD_CORPUS")
	if dir == "" {
		t.Skip("VALMIN_MOD_CORPUS not set; skipping the real-package placement suite")
	}

	zips, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(zips) == 0 {
		t.Fatalf("no .zip files in %s", dir)
	}

	for _, zp := range zips {
		t.Run(filepath.Base(zp), func(t *testing.T) {
			fullName, ok := corpusFullName(zp)
			if !ok {
				t.Fatalf("%s is not named Namespace-Name-Version.zip", filepath.Base(zp))
			}

			staging := t.TempDir()
			if err := extract.Extract(zp, staging); err != nil {
				t.Fatalf("Extract: %v", err)
			}
			placements, err := Plan(staging, fullName)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(placements) == 0 {
				t.Fatal("Plan placed nothing")
			}

			got := make([]string, 0, len(placements))
			for _, p := range placements {
				if strings.HasPrefix(p.Dest, "/") || strings.HasPrefix(p.Dest, "../") {
					t.Errorf("%s escapes the server root", p.Dest)
				}
				got = append(got, p.Dest)
			}
			for _, want := range corpusExpectations[fullName] {
				if !slices.Contains(got, want) {
					t.Errorf("missing %q; placed %d files under %v", want, len(got), destRoots(got))
				}
			}
			t.Logf("%s -> %d files under %v", fullName, len(got), destRoots(got))
		})
	}
}

// TestPlanPlacesNoWrapperDirectory is heuristic 1's negative half, kept separate because it
// asserts the absence of something: the framework pack's own wrapper name must appear in no
// destination, or every file lands one directory too deep and Doorstop finds nothing.
func TestPlanPlacesNoWrapperDirectory(t *testing.T) {
	dir := os.Getenv("VALMIN_MOD_CORPUS")
	if dir == "" {
		t.Skip("VALMIN_MOD_CORPUS not set; skipping the real-package placement suite")
	}
	zips, err := filepath.Glob(filepath.Join(dir, "denikson-BepInExPack_Valheim-*.zip"))
	if err != nil || len(zips) == 0 {
		t.Skip("the BepInEx pack is not in the corpus")
	}

	staging := t.TempDir()
	if err := extract.Extract(zips[0], staging); err != nil {
		t.Fatal(err)
	}
	placements, err := Plan(staging, "denikson-BepInExPack_Valheim")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range placements {
		if strings.Contains(p.Dest, "BepInExPack_Valheim") {
			t.Errorf("%s still carries the wrapper directory", p.Dest)
		}
	}
}

// corpusFullName turns "Namespace-Name-1.2.3.zip" into "Namespace-Name", reusing the
// resolver's own splitter so the two never disagree about where a version begins.
func corpusFullName(zipPath string) (string, bool) {
	base := strings.TrimSuffix(filepath.Base(zipPath), ".zip")
	fullName, _, ok := resolver.ParseDependency(base)
	return fullName, ok
}

// destRoots is the distinct destination directory of each placed file — the classification
// as a human reads it off the table. Directories rather than a leading segment because
// BepInEx/plugins/ and BepInEx/config/ are two heuristics landing in one tree, and
// collapsing them to "BepInEx" is the blindness F1 came from.
func destRoots(dests []string) []string {
	seen := map[string]bool{}
	for _, d := range dests {
		seen[path.Dir(d)] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// TestInstallThenUninstallOverTheCorpusIsByteIdentical is `05` M2's third "Done when" and
// WP-M2-09's first criterion, over real packages: every available package is installed into
// one server root — so BepInEx/plugins/ genuinely holds several packages' files at once,
// which is the case Q8's first data point says the heuristics cannot separate — and then
// removed from its own manifest. The tree has to come back to what it was, byte for byte.
//
// Skips without VALMIN_MOD_CORPUS, like every other real-package suite (ADR-105).
func TestInstallThenUninstallOverTheCorpusIsByteIdentical(t *testing.T) {
	dir := os.Getenv("VALMIN_MOD_CORPUS")
	if dir == "" {
		t.Skip("VALMIN_MOD_CORPUS not set; skipping the real-package round-trip suite")
	}
	zips, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(zips) == 0 {
		t.Fatalf("no .zip files in %s", dir)
	}

	serverRoot := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")
	// What was there before any mod: the game, and a config file an admin wrote by hand
	// that a package also ships — the skip case, which must survive the whole round trip.
	for rel, body := range map[string]string{
		"valheim_server.x86_64":                          "the game",
		"BepInEx/config/TherzieTranslations/Warfare.yml": "the admin's",
	} {
		p := filepath.Join(serverRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := treeHash(t, serverRoot)

	claims := map[string]string{}
	manifests := map[string][]ManifestEntry{}
	var order []string
	for _, zp := range zips {
		fullName, ok := corpusFullName(zp)
		if !ok || manifests[fullName] != nil {
			continue // not a package archive, or a second version of one already installed
		}
		staging := t.TempDir()
		if err := extract.Extract(zp, staging); err != nil {
			t.Fatalf("%s: Extract: %v", fullName, err)
		}
		placements, err := Plan(staging, fullName)
		if err != nil {
			t.Fatalf("%s: Plan: %v", fullName, err)
		}
		changes, err := Diff(fullName, placements, serverRoot, claims)
		var conflict *ConflictError
		if errors.As(err, &conflict) {
			t.Logf("%s: skipped, %s is owned by %s", fullName, conflict.Path, conflict.Owner)
			continue
		}
		if err != nil {
			t.Fatalf("%s: Diff: %v", fullName, err)
		}
		manifest, err := Manifest(changes)
		if err != nil {
			t.Fatalf("%s: Manifest: %v", fullName, err)
		}
		if err := Backup(changes, serverRoot, backupDir); err != nil {
			t.Fatalf("%s: Backup: %v", fullName, err)
		}
		if err := Apply(changes, serverRoot); err != nil {
			t.Fatalf("%s: Apply: %v", fullName, err)
		}
		for _, e := range manifest {
			claims[e.Path] = fullName
		}
		manifests[fullName] = manifest
		order = append(order, fullName)
	}
	if len(order) < 2 {
		t.Fatalf("installed %d packages; the shared-directory case needs at least two", len(order))
	}
	if got := treeHash(t, serverRoot); got == before {
		t.Fatal("installing the corpus changed nothing; the round trip proves nothing")
	}

	// Removed in reverse, which is the order that would expose a package deleting a
	// directory another one is still using.
	for i := len(order) - 1; i >= 0; i-- {
		if err := Remove(Paths(manifests[order[i]]), serverRoot); err != nil {
			t.Errorf("%s: Remove: %v", order[i], err)
		}
	}
	if got := treeHash(t, serverRoot); got != before {
		t.Errorf("server/ after uninstalling %d packages:\n%s\nwant:\n%s", len(order), got, before)
	}
}
