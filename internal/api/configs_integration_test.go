//go:build integration

// The config editor's API leg against a real daemon and the stub image: a mod installed, a
// server booted once so its settings file is written by something other than the test, and
// then every write path the panel offers, checked on disk rather than through its own
// response.
package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/store"
)

// rawConfigURL is this instance's raw route for one file.
func rawConfigURL(name, file string) string {
	return "/api/v1/instances/" + name + "/configs/" + file + "/raw"
}

// TestConfigEditingRoundTripsThroughTheAPI is the milestone's guarantee stated as a
// request: read a generated file, change one setting, and find on disk exactly the line that
// was asked for, the previous bytes in the .bak, and everything else untouched.
//
// The file is not a fixture. It is written by the stub at boot, one per plugin the installer
// actually placed, so nothing in this test tells the daemon what it is about to parse.
func TestConfigEditingRoundTripsThroughTheAPI(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	name := "cfg-edit-" + nameSuffix()
	dataDir := moddedInstance(t, rt, db, d, name)

	corpusIndexFromFixtures(t, db, threeDeep())
	installOneMod(t, rt, admin, name)
	openTree(t, filepath.Join(dataDir, "server"))

	if final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+name+"/start"); final.Status != "succeeded" {
		t.Fatalf("start job = %+v, want succeeded", final)
	}
	// Stopped, not merely started: every config write is refused on a running server
	// (C19, ADR-012), and the boot is only here to generate the file.
	if final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+name+"/stop"); final.Status != "succeeded" {
		t.Fatalf("stop job = %+v, want succeeded", final)
	}

	file := firstGeneratedConfig(t, rt, admin, name)
	path := filepath.Join(dataDir, "server", "BepInEx", "config", file)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the generated file: %v", err)
	}

	schema := readSchema(t, rt, admin, name, file)
	if schema.Plugin == "" {
		t.Error("the schema carries no plugin; the file's own header names one")
	}
	field, ok := boolField(schema)
	if !ok {
		t.Fatalf("no boolean setting in %s to edit", file)
	}

	patch := as(rt, admin, httptest.NewRequest(http.MethodPatch,
		"/api/v1/instances/"+name+"/configs/"+file, jsonBody(t, map[string]any{field: false})))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", patch.Code, patch.Body)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the edited file: %v", err)
	}
	assertOnlyTheseLinesChanged(t, before, after, 1)
	if got := config.Parse(after); !bytes.Equal(got.Bytes(), after) {
		t.Error("the file the daemon wrote does not round-trip")
	}

	// B4 and 03 §9 rule 5: the bytes a write replaced are kept, exactly.
	if kept, err := os.ReadFile(path + ".bak"); err != nil {
		t.Errorf("no .bak beside the edited file: %v", err)
	} else if !bytes.Equal(kept, before) {
		t.Error(".bak does not hold the bytes the write replaced")
	}

	// A raw replacement over the same file, on the ETag the read handed out — the escape
	// hatch and the typed path have to leave the same trail.
	read := as(rt, admin, httptest.NewRequest(http.MethodGet,
		rawConfigURL(name, file), http.NoBody))
	if read.Code != http.StatusOK {
		t.Fatalf("raw read = %d, want 200 (%s)", read.Code, read.Body)
	}
	etag := read.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the raw read sent no ETag, so a replacement cannot be guarded (11 §1.1)")
	}
	replacement := string(after) + "\n# added by the raw editor\n"

	stale := as(rt, admin, rawPut(rawConfigURL(name, file), replacement, `"not-the-etag"`))
	if stale.Code != http.StatusPreconditionFailed {
		t.Errorf("raw put on a stale ETag = %d, want 412 (%s)", stale.Code, stale.Body)
	}

	put := as(rt, admin, rawPut(rawConfigURL(name, file), replacement, etag))
	if put.Code != http.StatusOK {
		t.Fatalf("raw put = %d, want 200 (%s)", put.Code, put.Body)
	}
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != replacement {
		t.Error("the raw put did not land byte for byte")
	}
	if kept, err := os.ReadFile(path + ".bak"); err != nil {
		t.Errorf("no .bak after a raw put: %v", err)
	} else if !bytes.Equal(kept, after) {
		t.Error("the raw put's .bak does not hold what it replaced")
	}
	// The copy taken before the panel's first write is the one that survives every later
	// one, which is what makes "compare with the original" mean anything after two saves.
	if orig, err := os.ReadFile(path + ".orig"); err != nil {
		t.Errorf("no .orig: %v", err)
	} else if !bytes.Equal(orig, before) {
		t.Error(".orig moved with the second write; it must hold the file as first found")
	}
}

// installOneMod installs a package from the seeded index and waits for the job.
func installOneMod(t *testing.T, rt *Router, admin *store.User, name string) {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/"+name+"/mods",
		jsonBody(t, installBody("OdinPlus-OdinArchitect", "1.7.0"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("install = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitForJobTerminal(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("install job = %+v, want succeeded", got)
	}
}

// firstGeneratedConfig names a file the boot produced, and fails if the boot produced none —
// which is the whole premise of this test rather than a precondition worth skipping over.
func firstGeneratedConfig(t *testing.T, rt *Router, admin *store.User, name string) string {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/"+name+"/configs", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list configs = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var list configListView
	decodeInto(t, rec, &list)
	for _, item := range list.Items {
		// BepInEx.cfg is the framework's own, written by the panel (ADR-108). The subject
		// here is a file the server wrote.
		if item.File != "BepInEx.cfg" {
			return item.File
		}
	}
	t.Fatalf("the boot generated no plugin config; list = %+v", list)
	return ""
}

func readSchema(t *testing.T, rt *Router, admin *store.User, name, file string) config.Schema {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/"+name+"/configs/"+file, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("read schema = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var schema config.Schema
	decodeInto(t, rec, &schema)
	return schema
}

// boolField names a toggle in the schema, which is the one widget whose other value needs no
// knowledge of the setting.
func boolField(schema config.Schema) (string, bool) {
	for _, section := range schema.Sections {
		for _, item := range section.Settings {
			if current, ok := item.Current.(bool); ok && current {
				return section.Name + "." + item.Key, true
			}
		}
	}
	return "", false
}

// assertOnlyTheseLinesChanged is the round-trip guarantee as an assertion over two files: the
// line count is unmoved and exactly want lines differ.
func assertOnlyTheseLinesChanged(t *testing.T, before, after []byte, want int) {
	t.Helper()
	was := strings.Split(string(before), "\n")
	is := strings.Split(string(after), "\n")
	if len(was) != len(is) {
		t.Fatalf("line count went from %d to %d", len(was), len(is))
	}
	changed := 0
	for i := range was {
		if was[i] != is[i] {
			changed++
		}
	}
	if changed != want {
		t.Errorf("%d lines changed, want %d", changed, want)
	}
}
