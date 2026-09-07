//go:build integration

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

func TestGameUpdateReplaysManifestAndPreservesWorldAndConfigs(t *testing.T) {
	if os.Getuid() != instance.WantCloneUID {
		t.Skip("successful game update requires uid 10000; run make test-integration-as-panel")
	}
	rt, db, docker, admin := lifecycleRouter(t)
	seedRealInstance(t, rt, db, docker, seededInstanceID)
	seedWorldOnDisk(t, db)
	inst, err := db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil || inst == nil {
		t.Fatalf("read instance: row=%v err=%v", inst, err)
	}
	dataDir := inst.DataDir
	files := map[string]string{
		"doorstop_libs/libdoorstop_x64.so":    "synthetic loader",
		"BepInEx/plugins/custom/Recorded.dll": "synthetic plugin",
		"BepInEx/config/Package.cfg":          "packaged default",
	}
	var manifest []installer.ManifestEntry
	archive := make(map[string]string)
	archive["BepInEx/plugins/Unmanifested.dll"] = "must never be replayed"
	for path, body := range files {
		sum := sha256.Sum256([]byte(body))
		manifest = append(manifest, installer.ManifestEntry{Path: path, SHA256: hex.EncodeToString(sum[:])})
		archive["unrelated-layout/"+filepath.Base(path)] = body
		writeServerFile(t, dataDir, path, body)
	}
	corpusIndexFromFixtures(t, db, []modPackageFixture{{
		fullName: "Fixture-Replay", version: "1.0.0", deps: []string{}, files: archive,
	}})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteInstanceMods(t.Context(), seededInstanceID, []store.InstanceMod{{
		InstanceID: seededInstanceID, FullName: "Fixture-Replay", Version: "1.0.0",
		InstalledAs: store.InstalledExplicit, Side: store.SideUnknown, Enabled: true,
		FileManifest: string(encoded),
	}}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"Package.cfg":     "# operator comment\nSetting = edited\n",
		"Package.cfg.bak": "previous edit", "Package.cfg.orig": "original bytes",
		"nested/untracked.cfg": "untracked user configuration",
	} {
		writeServerFile(t, dataDir, "BepInEx/config/"+name, body)
	}
	writeServerFile(t, dataDir, "obsolete-game-file", "old build")
	worlds := acceptanceWorldTree(t, worldsDirOf(t, db))
	configs := acceptanceWorldTree(t, filepath.Join(dataDir, "server/BepInEx/config"))
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, updatePath(seededInstanceID),
		jsonBody(t, map[string]bool{"confirm_modded": true})))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if final := waitForJobTerminal(t, rt, admin, accepted.JobID); final.Status != "succeeded" {
		t.Fatalf("update = %+v", final)
	}
	if got := acceptanceWorldTree(t, worldsDirOf(t, db)); !maps.Equal(got, worlds) {
		t.Error("update changed world bytes or paths")
	}
	if got := acceptanceWorldTree(t, filepath.Join(dataDir, "server/BepInEx/config")); !maps.Equal(got, configs) {
		t.Error("update changed user config bytes or paths")
	}
	buildID, err := instance.InstalledBuildID(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	expected := acceptanceWorldTree(t, filepath.Join(instance.CacheDir(rt.Supervisor().inst.Cfg.Data.Root), buildID))
	for name := range expected {
		if strings.HasSuffix(name, "/") {
			delete(expected, name)
		}
	}
	maps.Copy(expected, files)
	for name, body := range configs {
		if !strings.HasSuffix(name, "/") {
			expected["BepInEx/config/"+name] = body
		}
	}
	actual := acceptanceWorldTree(t, filepath.Join(dataDir, "server"))
	for name := range actual {
		if strings.HasSuffix(name, "/") {
			delete(actual, name)
		}
	}
	if !maps.Equal(actual, expected) {
		t.Error("updated server files differ from cached game, manifested files and preserved configs")
	}
	for _, entry := range manifest {
		if entry.Path == "BepInEx/config/Package.cfg" {
			continue // User configuration overrides the package default.
		}
		body, err := os.ReadFile(filepath.Join(dataDir, "server", entry.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Errorf("replayed %s differs from its manifest hash", entry.Path)
		}
	}
	for _, name := range []string{"obsolete-game-file", "unrelated-layout"} {
		if _, err := os.Stat(filepath.Join(dataDir, "server", name)); !os.IsNotExist(err) {
			t.Errorf("unexpected server path %s: %v", name, err)
		}
	}
	if got := instanceState(t, rt, admin, seededInstanceID); got != "stopped" {
		t.Errorf("update left instance %q", got)
	}
	// By label, not by a container id: the update leaves the container alone, and the id the
	// row carries stops being the live one as soon as a start rebuilds the drifted spec
	// (ADR-118). The count is asserted so "nothing running" cannot pass on nothing existing.
	found, err := docker.List(t.Context(), map[string]string{instance.LabelInstanceID: seededInstanceID})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("instance has %d containers after the update, want 1", len(found))
	}
	if found[0].Running {
		t.Fatalf("update started its container: %+v", found[0])
	}
	if snapshots := backupsWithTrigger(t, rt, admin, store.TriggerPreUpdate); len(snapshots) != 1 {
		t.Errorf("got %d pre-update archives, want 1", len(snapshots))
	}
}
