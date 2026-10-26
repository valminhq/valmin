package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/export"
	"github.com/valminhq/valmin/internal/store"
)

// Asserts only an admin reaches the export, which answers 404 until one exists, and that a
// download names the latest one.
func TestTheExportIsAnAdminDownload(t *testing.T) {
	t.Parallel()
	_, db, admin, member := world(t)
	h := &Exports{DB: db, Authz: authz.New(db), Exporter: &export.Exporter{DB: db, Root: t.TempDir()}}
	call := func(handler http.HandlerFunc, u *store.User) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		handler.ServeHTTP(rec, r.WithContext(middleware.WithUser(r.Context(), u)))
		return rec
	}

	if rec := call(h.latest, member); rec.Code != http.StatusNotFound {
		t.Errorf("member GET /admin/export = %d, want 404", rec.Code)
	}
	if rec := call(h.download, admin); rec.Code != http.StatusNotFound {
		t.Errorf("download with no export = %d, want 404", rec.Code)
	}

	dir := export.Dir(h.Exporter.Root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	name := "valmin-export-full-20261025T120000Z.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := call(h.latest, admin)
	var view latestExportView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.Latest == nil ||
		view.Latest.Filename != name || view.Latest.Mode != "full" || view.Latest.SizeBytes != 7 {
		t.Errorf("GET /admin/export = %d %s, want the full export", rec.Code, rec.Body)
	}
	if rec := call(h.download, member); rec.Code != http.StatusNotFound {
		t.Errorf("member download = %d, want 404", rec.Code)
	}
	if rec := call(h.download, admin); rec.Code != http.StatusOK || rec.Body.String() != "archive" {
		t.Errorf("admin download = %d %q, want the archive", rec.Code, rec.Body)
	}
}
