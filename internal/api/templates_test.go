package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/sharecode"
	"github.com/valminhq/valmin/internal/store"
)

// templateArchive is a repository archive holding one template.
func templateArchive(t *testing.T) []byte {
	t.Helper()
	code, err := sharecode.Encode(&sharecode.Template{
		Name: "QoL", Mods: []control.ManifestMod{{FullName: "Azumatt-AzuCraftyBoxes", Version: "1.5.3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("name: Vanilla QoL\ndescription: Less juggling.\ncode: " + code + "\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "r/templates/vanilla-qol/template.yaml", Mode: 0o644, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func listTemplatesAs(t *testing.T, h *Templates, u *store.User, query string) (int, templateCatalogueView) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates"+query, http.NoBody)
	rec := httptest.NewRecorder()
	h.list(rec, req.WithContext(middleware.WithUser(req.Context(), u)))
	var view templateCatalogueView
	if rec.Code == http.StatusOK {
		decodeInto(t, rec, &view)
	}
	return rec.Code, view
}

// TestTemplateCatalogue asserts a repository is fetched once per refresh interval, refetched on
// request, keeps its last good templates when a refetch fails, and is hidden from a member.
func TestTemplateCatalogue(t *testing.T) {
	t.Parallel()
	rt, _, _, admin, member := lifecycleWorld(t)
	body := templateArchive(t)
	var hits atomic.Int32
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	h := &Templates{Authz: rt.instances.Authz, Sources: []string{srv.URL}, Client: srv.Client()}

	check := func(step, query string, wantHits int32, wantErr bool) {
		t.Helper()
		code, view := listTemplatesAs(t, h, admin, query)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", step, code)
		}
		if len(view.Templates) != 1 || view.Templates[0].Name != "Vanilla QoL" ||
			view.Templates[0].Slug != "vanilla-qol" || view.Templates[0].Mods != 1 {
			t.Fatalf("%s: templates = %+v", step, view.Templates)
		}
		if got := hits.Load(); got != wantHits {
			t.Errorf("%s: %d fetches, want %d", step, got, wantHits)
		}
		if gotErr := view.Sources[0].Error != nil; gotErr != wantErr {
			t.Errorf("%s: source error = %v, want error %v", step, view.Sources[0].Error, wantErr)
		}
	}
	check("first open", "", 1, false)
	check("cached", "", 1, false)
	check("refresh, unchanged", "?refresh=true", 2, false)
	failing.Store(true)
	check("refresh, failing", "?refresh=true", 3, true)

	if code, _ := listTemplatesAs(t, h, member, ""); code != http.StatusForbidden {
		t.Errorf("member: status %d, want 403", code)
	}
}
