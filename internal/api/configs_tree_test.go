package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/store"
)

// seedConfigTree writes files, keyed by slash path, under the instance's config directory and
// returns that directory.
func seedConfigTree(t *testing.T, rt *Server, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(rt.instances.Cfg.Data.HostRoot,
		"instances", seededInstanceID, "server", "BepInEx", "config")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// nestedRawURL is the raw route of a config path, with its slashes escaped as the SPA sends them.
func nestedRawURL(name string) string {
	return configURL("/" + url.PathEscape(name) + "/raw")
}

// putNestedRaw replaces a config through the raw route, reading its ETag first.
func putNestedRaw(t *testing.T, rt *Server, u *store.User, name, content string) *httptest.ResponseRecorder {
	t.Helper()
	get := as(rt, u, httptest.NewRequest(http.MethodGet, nestedRawURL(name), http.NoBody))
	if get.Code != http.StatusOK {
		t.Fatalf("raw get %s = %d (%s)", name, get.Code, get.Body)
	}
	return as(rt, u, rawPut(nestedRawURL(name), content, get.Header().Get("ETag")))
}

// TestConfigListWalksSubdirectories asserts the list reaches config files in subdirectories,
// leaves out hidden, binary, kept, too deep and too large files with a note, and marks only
// what saved setups carry as portable.
func TestConfigListWalksSubdirectories(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	dir := seedConfigTree(t, rt, map[string]string{
		seededConfigFile:                      seededConfig,
		"expand_world/expand_biomes.yaml":     "biomes: []\n",
		"expand_world/expand_biomes.yaml.bak": "old\n",
		"EpicLoot/patches/a.json":             "{}",
		"notes.txt":                           "hi",
		"spawn_that/extra.cfg":                "[A]\nk = 1\n",
		".hidden/x.yaml":                      "a: 1\n",
		".valmin-tmp":                         "",
		"icons/a.png":                         "png",
		"a/b/c/d/e/f/g/h/ok.yaml":             "a: 1\n",
		"a/b/c/d/e/f/g/h/i/deep.yaml":         "a: 1\n",
		"big.json":                            strings.Repeat(" ", instance.MaxConfigSize+1),
		"big.cfg":                             strings.Repeat("#\n", instance.MaxConfigSize),
	})
	for link, target := range map[string]string{
		"linked.cfg": seededConfigFile,
		"escape.cfg": filepath.Join(t.TempDir(), "outside.cfg"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, configURL(""), http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body)
	}
	var body configListView
	decodeInto(t, rec, &body)
	portable := map[string]bool{}
	for _, item := range body.Items {
		portable[item.File] = item.Portable
	}
	want := map[string]bool{
		seededConfigFile:                  true,
		"big.cfg":                         true,
		"linked.cfg":                      true,
		"EpicLoot/patches/a.json":         false,
		"a/b/c/d/e/f/g/h/ok.yaml":         false,
		"expand_world/expand_biomes.yaml": false,
		"notes.txt":                       false,
		"spawn_that/extra.cfg":            false,
	}
	if !reflect.DeepEqual(portable, want) {
		t.Errorf("listed (file: portable) = %v, want %v", portable, want)
	}
	if body.Note != skippedConfigNote {
		t.Errorf("note = %q, want the note naming the limits", body.Note)
	}
}

// TestANestedPathThroughAFileIsNotFound asserts a path whose folder is really a file reads as
// missing rather than failing.
func TestANestedPathThroughAFileIsNotFound(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigFile(t, rt)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, nestedRawURL(seededConfigFile+"/x.yaml"), http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestRawEditOfANestedFile asserts a file in a subdirectory is read and replaced through the raw
// route with its copies kept beside it, and that only a .cfg gets a pending copy on a running
// server.
func TestRawEditOfANestedFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		wantPending bool
	}{
		{"expand_world/expand_biomes.yaml", false},
		{"mod/extra.cfg", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, db, fake, admin, _ := lifecycleWorld(t)
			seedInstance(t, rt, db, fake, instanceStateRunning)
			dir := seedConfigTree(t, rt, map[string]string{tt.name: "a: 1\n"})
			path := filepath.Join(dir, filepath.FromSlash(tt.name))

			if rec := putNestedRaw(t, rt, admin, tt.name, "a: 2\n"); rec.Code != http.StatusOK {
				t.Fatalf("raw put = %d (%s)", rec.Code, rec.Body)
			}
			if got := readFile(t, path); got != "a: 2\n" {
				t.Errorf("file = %q, want the new content", got)
			}
			prev := as(rt, admin, httptest.NewRequest(http.MethodGet,
				configURL("/"+url.PathEscape(tt.name)+"/previous/raw"), http.NoBody))
			if prev.Code != http.StatusOK || prev.Body.String() != "a: 1\n" {
				t.Errorf("previous = %d %q, want the replaced content", prev.Code, prev.Body)
			}
			_, err := os.Stat(path + modconfig.PendingSuffix)
			if pending := err == nil; pending != tt.wantPending {
				t.Errorf("pending copy = %v, want %v", pending, tt.wantPending)
			}
		})
	}
}

// TestRawPutChecksJSONAndYAMLSyntax asserts a write that breaks a JSON or YAML file is refused
// with a 422 naming the problem, even when the file on disk already failed the check.
func TestRawPutChecksJSONAndYAMLSyntax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, current, next string
		wantCode            int
	}{
		{"x.json", `{"a": 1}`, `{"a": }`, http.StatusUnprocessableEntity},
		{"x.json", `{"a": 1}`, "\xef\xbb\xbf{\"a\": 2}", http.StatusOK},
		{"x.json", "{\n// a comment\n\"a\": 1}", "{\n// a comment\n\"a\": 2}", http.StatusUnprocessableEntity},
		{"x.json", "{\n// a comment\n\"a\": 1}", `{"a": 2}`, http.StatusOK},
		{"x.yaml", "a: 1\n", "a: [1\n", http.StatusUnprocessableEntity},
		{"x.yml", "a: 1\n", "a: 1\n---\nb: : c\n", http.StatusUnprocessableEntity},
		{"x.yml", "a: 1\n", "a: 1\n---\nb: 2\n", http.StatusOK},
		{"x.txt", "a", "{", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name+" "+tt.next, func(t *testing.T) {
			t.Parallel()
			rt, db, fake, admin, _ := lifecycleWorld(t)
			seedInstance(t, rt, db, fake, "stopped")
			dir := seedConfigTree(t, rt, map[string]string{tt.name: tt.current})

			rec := putNestedRaw(t, rt, admin, tt.name, tt.next)
			if rec.Code != tt.wantCode {
				t.Fatalf("raw put = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body)
			}
			want := tt.next
			if tt.wantCode != http.StatusOK {
				want = tt.current
				if !strings.Contains(rec.Body.String(), `"content"`) {
					t.Errorf("422 does not name the content field: %s", rec.Body)
				}
			}
			if got := readFile(t, filepath.Join(dir, tt.name)); got != want {
				t.Errorf("file = %q, want %q", got, want)
			}
		})
	}
}

// TestSettingsRoutesServeOnlyCfgFiles asserts the typed read, patch and copy routes refuse a
// text-only file, which has no settings to project.
func TestSettingsRoutesServeOnlyCfgFiles(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigTree(t, rt, map[string]string{"expand_world/x.yaml": "a = 1\n"})
	file := "/" + url.PathEscape("expand_world/x.yaml")

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, configURL(file), http.NoBody),
		httptest.NewRequest(http.MethodGet, configURL(file+"/original"), http.NoBody),
		httptest.NewRequest(http.MethodPatch, configURL(file), jsonBody(t, map[string]any{"a": 2})),
	} {
		if rec := as(rt, admin, req); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.Method, req.URL, rec.Code)
		}
	}
}

// TestTextFileWriteAuditListsNoChanges asserts a raw write to a text-only file is audited by file
// and size alone, since the settings diff would read its lines as bogus settings.
func TestTextFileWriteAuditListsNoChanges(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigTree(t, rt, map[string]string{"x.yaml": "url: http://x?token=a\n"})

	if rec := putNestedRaw(t, rt, admin, "x.yaml", "url: http://x?token=b\n"); rec.Code != http.StatusOK {
		t.Fatalf("raw put = %d (%s)", rec.Code, rec.Body)
	}
	detail, stored := writeAuditOf(t, db)
	if detail.File != "x.yaml" || !detail.Raw || len(detail.Changes) != 0 {
		t.Errorf("audit detail = %+v, want the file, raw and no changes", detail)
	}
	if strings.Contains(stored, "token=") {
		t.Errorf("audit detail %s records the file's content", stored)
	}
}
