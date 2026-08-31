package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func builtSPA() fs.FS {
	return fstest.MapFS{
		"build/app/index.html":                       {Data: []byte("<!doctype html><title>Valmin</title>")},
		"build/app/_app/immutable/entry/app.hash.js": {Data: []byte("export const app = 1;")},
		"build/app/favicon.svg":                      {Data: []byte("<svg/>")},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec
}

// TestAClientSideRouteSurvivesAHardRefresh is what the fallback exists for: a SPA route has
// no file behind it, and a browser asked for it directly still has to get the app.
func TestAClientSideRouteSurvivesAHardRefresh(t *testing.T) {
	h := SPA(builtSPA())

	for _, path := range []string{"/", "/login", "/setup", "/instances/abc/console"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<title>Valmin</title>") {
			t.Errorf("GET %s did not serve the app: %q", path, rec.Body.String())
		}
	}
}

func TestAssetsAreServedAndHashedOnesCachedForever(t *testing.T) {
	h := SPA(builtSPA())

	asset := get(t, h, "/_app/immutable/entry/app.hash.js")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "export const app") {
		t.Fatalf("hashed asset = %d %q", asset.Code, asset.Body.String())
	}
	// `↯` The name carries a content hash, so it can be cached forever — and index.html
	// must not be, or a deploy leaves browsers running the previous app against the new API.
	if got := asset.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q", got)
	}
	if got := get(t, h, "/").Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("index.html Cache-Control = %q; it must be revalidated", got)
	}
}

// TestAnUnbuiltBinarySaysSoRatherThanCrashing: `go build` without `make build` is a normal
// thing to do while working on the daemon, and the API is fine — so the answer is a page
// naming the fix, not a 500 that reads like a bug.
func TestAnUnbuiltBinarySaysSoRatherThanCrashing(t *testing.T) {
	rec := get(t, SPA(fstest.MapFS{"build/.gitignore": {Data: []byte("app/\n")}}), "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make build") {
		t.Errorf("the page does not say how to fix it: %q", rec.Body.String())
	}
}
