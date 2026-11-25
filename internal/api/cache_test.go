package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// Asserts only an admin sees the inventory, and that an unused zip inside its grace period is
// listed as prunable later with nothing reclaimable yet.
func TestTheCacheInventoryIsAnAdminView(t *testing.T) {
	t.Parallel()
	_, db, admin, member := world(t)
	root := t.TempDir()
	caches := map[source.Source]*cache.Cache{}
	for _, src := range source.All() {
		caches[src] = cache.New(cache.RootFor(root, src))
	}
	zips := cache.RootFor(root, source.Thunderstore)
	if err := os.MkdirAll(zips, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zips, "Some-Mod-1.0.0.zip"), []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Caches{Authz: authz.New(db), Pruner: &control.CachePruner{
		DB: db, DataRoot: root, Builds: instance.NewBuildCache(), Caches: caches, Now: time.Now,
	}}
	call := func(u *store.User) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		h.inventory(rec, r.WithContext(middleware.WithUser(r.Context(), u)))
		return rec
	}

	if rec := call(member); rec.Code != http.StatusNotFound {
		t.Errorf("member GET /admin/cache = %d, want 404", rec.Code)
	}
	rec := call(admin)
	var view cacheInventoryView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("admin GET /admin/cache = %d %s", rec.Code, rec.Body)
	}
	if len(view.Summary) != 4 || view.GraceSeconds != 3600 || view.NextCursor != nil {
		t.Errorf("view = %+v, want four kinds, an hour's grace and one page", view)
	}
	if len(view.Items) != 1 {
		t.Fatalf("items = %+v, want the one zip", view.Items)
	}
	if len(view.Summary) != 4 {
		t.Fatalf("summary = %+v, want four kinds", view.Summary)
	}
	if m := view.Summary[0]; m.Kind != "mod_archive" || m.Count != 1 || m.SizeBytes <= 0 ||
		m.SizeBytes != view.Items[0].SizeBytes {
		t.Errorf("mod_archive summary = %+v, want the zip counted and sized", m)
	}
	item := view.Items[0]
	if item.Kind != "mod_archive" || item.Name != "Some-Mod-1.0.0" || item.Registry == nil ||
		*item.Registry != "thunderstore" || item.InUse || item.PrunableAt == nil || item.ReclaimableBytes != 0 {
		t.Errorf("item = %+v, want an unused thunderstore zip prunable later", item)
	}
}
