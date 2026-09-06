package instance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/backup"
)

// buildManifestID is the build the checked-in appmanifest fixture declares.
const buildManifestID = "21981590"

// updateWorld is an instance directory with a cached build to update to, a live server tree
// carrying a mod and an edited config, and a world that must come through untouched.
type updateWorld struct {
	dataDir   string
	cacheRoot string
}

func newUpdateWorld(t *testing.T) updateWorld {
	t.Helper()
	root := t.TempDir()
	w := updateWorld{dataDir: filepath.Join(root, "instance"), cacheRoot: filepath.Join(root, "cache")}

	// The cached build: a manifest, the binary marker, and one shipped default config.
	cache := filepath.Join(w.cacheRoot, buildManifestID)
	writeSteamManifest(t, cache)
	writeAt(t, filepath.Join(cache, "valheim_server_Data", "shipped.dat"), "new build")
	writeAt(t, filepath.Join(cache, "BepInEx", "config", "shipped.cfg"), "shipped default")

	// The live server: an older build, a mod framework, and a config the operator edited.
	server := ServerDir(w.dataDir)
	writeSteamManifest(t, server)
	writeAt(t, filepath.Join(server, "valheim_server_Data", "shipped.dat"), "old build")
	writeAt(t, filepath.Join(server, "doorstop_libs", "libdoorstop_x64.so"), "doorstop")
	writeAt(t, filepath.Join(server, "BepInEx", "config", "shipped.cfg"), "the operator's edit")
	writeAt(t, filepath.Join(server, "BepInEx", "config", "shipped.cfg.orig"), "shipped default")
	writeAt(t, filepath.Join(server, "BepInEx", "config", "shipped.cfg.bak"), "a previous edit")

	writeAt(t, filepath.Join(WorldsDir(w.dataDir), WorldsLocalDir, "World.db"), "a world")
	return w
}

func writeAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o664); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// stageEverything runs the phases the job runs, minus the mod replay the api layer owns.
func (w updateWorld) stageEverything(t *testing.T) {
	t.Helper()
	if err := StageUpdate(t.Context(), w.dataDir, w.cacheRoot, buildManifestID); err != nil {
		t.Fatalf("StageUpdate: %v", err)
	}
	if err := SaveUpdateConfigs(w.dataDir); err != nil {
		t.Fatalf("SaveUpdateConfigs: %v", err)
	}
	if err := ApplyStagedFiles(w.dataDir); err != nil {
		t.Fatalf("ApplyStagedFiles: %v", err)
	}
}

// Asserts staging touches neither the live server nor the world: everything lands beside them,
// so a failure at any point before the swap costs nothing.
func TestStagingLeavesTheLiveServerAndWorldAlone(t *testing.T) {
	w := newUpdateWorld(t)
	w.stageEverything(t)

	live := filepath.Join(ServerDir(w.dataDir), "valheim_server_Data", "shipped.dat")
	if got := read(t, live); got != "old build" {
		t.Errorf("the live server holds %q, want it untouched until the swap", got)
	}
	world := filepath.Join(WorldsDir(w.dataDir), WorldsLocalDir, "World.db")
	if got := read(t, world); got != "a world" {
		t.Errorf("worlds/ holds %q; an update must not reach it at all (02 §3)", got)
	}
	staged := filepath.Join(StagedServerDir(w.dataDir), "valheim_server_Data", "shipped.dat")
	if got := read(t, staged); got != "new build" {
		t.Errorf("the staged server holds %q, want the new build", got)
	}
}

// Asserts the operator's config edits win over the build's shipped defaults, and that the
// saved copies a manifest cannot reconstruct come through byte for byte (ADR-125, ADR-138).
func TestTheUsersConfigsSurviveTheNewBuild(t *testing.T) {
	w := newUpdateWorld(t)
	w.stageEverything(t)
	if err := SwapUpdate(w.dataDir); err != nil {
		t.Fatalf("SwapUpdate: %v", err)
	}

	config := filepath.Join(ServerDir(w.dataDir), "BepInEx", "config")
	for name, want := range map[string]string{
		"shipped.cfg":      "the operator's edit",
		"shipped.cfg.orig": "shipped default",
		"shipped.cfg.bak":  "a previous edit",
	} {
		if got := read(t, filepath.Join(config, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// Asserts the swap actually installs the new build and leaves neither staging directory
// behind: a stray server.new is a second copy of the game on the operator's disk.
func TestSwapInstallsTheNewBuild(t *testing.T) {
	w := newUpdateWorld(t)
	w.stageEverything(t)
	if err := SwapUpdate(w.dataDir); err != nil {
		t.Fatalf("SwapUpdate: %v", err)
	}

	got := read(t, filepath.Join(ServerDir(w.dataDir), "valheim_server_Data", "shipped.dat"))
	if got != "new build" {
		t.Errorf("server/ holds %q after the swap, want the new build", got)
	}
	for _, suffix := range []string{backup.StagedSuffix, backup.SupersededSuffix} {
		if _, err := os.Stat(ServerDir(w.dataDir) + suffix); !os.IsNotExist(err) {
			t.Errorf("server%s survived the swap", suffix)
		}
	}
}

// Asserts every point an update can be killed at resolves to exactly one server/ tree, and to
// the right one: before the renames the old build stays, between them the staged build lands.
func TestRecoverUpdateResolvesEveryInterruptedSwap(t *testing.T) {
	tests := []struct {
		name string
		// kill runs at the point the panel died, leaving the directories as they were.
		kill func(t *testing.T, w updateWorld)
		want string
	}{
		{"during the replay, before any rename", func(t *testing.T, w updateWorld) {
			w.stageEverything(t)
		}, "old build"},
		{"between the two renames", func(t *testing.T, w updateWorld) {
			w.stageEverything(t)
			live := ServerDir(w.dataDir)
			if err := os.Rename(live, live+backup.SupersededSuffix); err != nil {
				t.Fatal(err)
			}
		}, "new build"},
		{"after the second rename, before the cleanup", func(t *testing.T, w updateWorld) {
			w.stageEverything(t)
			if err := SwapUpdate(w.dataDir); err != nil {
				t.Fatal(err)
			}
			writeAt(t, filepath.Join(UpdateStaging(w.dataDir), "build-id"), buildManifestID)
		}, "new build"},
		{"before anything was staged at all", func(t *testing.T, w updateWorld) {}, "old build"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newUpdateWorld(t)
			tt.kill(t, w)

			if _, err := RecoverUpdate(w.dataDir); err != nil {
				t.Fatalf("RecoverUpdate: %v", err)
			}

			got := read(t, filepath.Join(ServerDir(w.dataDir), "valheim_server_Data", "shipped.dat"))
			if got != tt.want {
				t.Errorf("server/ holds %q after recovery, want %q", got, tt.want)
			}
			for _, leftover := range []string{
				StagedServerDir(w.dataDir),
				ServerDir(w.dataDir) + backup.SupersededSuffix,
				UpdateStaging(w.dataDir),
			} {
				if _, err := os.Stat(leftover); !os.IsNotExist(err) {
					t.Errorf("%s survived recovery", filepath.Base(leftover))
				}
			}
			world := filepath.Join(WorldsDir(w.dataDir), WorldsLocalDir, "World.db")
			if got := read(t, world); got != "a world" {
				t.Errorf("recovery changed worlds/: %q", got)
			}
		})
	}
}

// Asserts the staged build is recorded and readable, so an operator looking at a parked
// instance can be told which build the interrupted run was installing.
func TestStagedBuildIDNamesWhatWasBeingInstalled(t *testing.T) {
	w := newUpdateWorld(t)
	if got := StagedBuildID(w.dataDir); got != "" {
		t.Errorf("StagedBuildID with no update in flight = %q, want empty", got)
	}
	if err := StageUpdate(t.Context(), w.dataDir, w.cacheRoot, buildManifestID); err != nil {
		t.Fatal(err)
	}
	if got := StagedBuildID(w.dataDir); got != buildManifestID {
		t.Errorf("StagedBuildID = %q, want %q", got, buildManifestID)
	}
}

// Asserts staging refuses a build the cache cannot vouch for, before it clones anything: the
// directory name is not evidence, the manifest inside it is.
func TestStageUpdateRefusesABuildTheCacheCannotVouchFor(t *testing.T) {
	w := newUpdateWorld(t)

	for _, buildID := range []string{"", "latest", "../escape", "99999999"} {
		if err := StageUpdate(t.Context(), w.dataDir, w.cacheRoot, buildID); err == nil {
			t.Errorf("StageUpdate(%q) succeeded, want a refusal", buildID)
		}
	}
	if _, err := os.Stat(StagedServerDir(w.dataDir)); !os.IsNotExist(err) {
		t.Error("a refused build still staged a clone")
	}
}

// Asserts the modded test is the entrypoint's: Doorstop's library on disk, nothing else.
func TestHasDoorstopFollowsTheEntrypointsTest(t *testing.T) {
	w := newUpdateWorld(t)
	modded, err := HasDoorstop(w.dataDir)
	if err != nil || !modded {
		t.Fatalf("HasDoorstop = %v, %v; want true", modded, err)
	}

	if err := os.Remove(filepath.Join(ServerDir(w.dataDir), "doorstop_libs", "libdoorstop_x64.so")); err != nil {
		t.Fatal(err)
	}
	modded, err = HasDoorstop(w.dataDir)
	if err != nil || modded {
		t.Errorf("HasDoorstop after removing the library = %v, %v; want false", modded, err)
	}
}
