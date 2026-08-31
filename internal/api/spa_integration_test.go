//go:build integration

package api_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/api"
	"github.com/valminhq/valmin/web"
)

// TestTheBinaryCarriesTheBuiltSPA is WP-22's first acceptance criterion, and it lives under
// the integration tag for one reason: `make build` runs before `make test-integration` and
// after `make test`, so this is the only stage at which the SPA is guaranteed to have been
// built. Under `make test` it would skip on a clean tree and prove nothing.
//
// `↯` One artefact, no Node process in production (`02 §2.1`, ADR-002). The check is that
// the *embedded* filesystem — not a fixture — serves the app.
func TestTheBinaryCarriesTheBuiltSPA(t *testing.T) {
	entries, err := fs.ReadDir(web.Assets, "build/app")
	if err != nil || len(entries) == 0 {
		t.Fatalf("nothing is embedded: run `make build` before the integration suite (%v)", err)
	}

	h := api.SPA(web.Assets)
	for _, path := range []string{"/", "/login", "/setup"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want the embedded app", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<!doctype html>") {
			t.Errorf("GET %s served %q", path, rec.Body.String())
		}
	}

	// The hashed assets the app loads are embedded too, or the page arrives and then breaks.
	immutable, err := fs.ReadDir(web.Assets, "build/app/_app/immutable")
	if err != nil || len(immutable) == 0 {
		t.Fatalf("the app's own assets are not embedded: %v", err)
	}
}
