package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

func installRegistryFixture(t *testing.T, db *store.DB, name, version string, src source.Source) {
	t.Helper()
	err := db.WriteInstanceMods(t.Context(), "inst-a", []store.InstanceMod{{
		Source: src, InstanceID: "inst-a", FullName: name, Version: version,
		InstalledAs: store.InstalledExplicit, Side: store.SideUnknown, Enabled: true, FileManifest: "[]",
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveReportsInstalledRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	installRegistryFixture(t, db, "Only-Ts", "2.0.0", source.Thunderstore)
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{"full_name": "Only-Hex", "version": "1.0.0", "source": "hexium"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	var got resolveResponse
	decodeInto(t, rec, &got)
	for _, node := range got.Nodes {
		if node.FullName == "Only-Ts" {
			if node.Source != "thunderstore" || !node.NoOp {
				t.Errorf("installed dependency = %+v", node)
			}
			return
		}
	}
	t.Fatal("installed dependency missing")
}

func TestResolveRejectsInstalledRegistryReplacement(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	installRegistryFixture(t, db, "Only-Ts", "2.0.0", source.Thunderstore)
	err := db.UpsertModPackages(t.Context(), nil, []store.ModVersion{{
		FullName: "Only-Ts", Source: source.Hexium, Version: "3.0.0", DependenciesJSON: "[]",
	}})
	if err != nil {
		t.Fatal(err)
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{"full_name": "Only-Ts", "version": "3.0.0", "source": "hexium"})))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cross-registry replacement: status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

func TestDisabledRegistryCannotSupplyInstall(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	delete(rt.mods.Clients, source.Hexium)
	delete(rt.mods.Caches, source.Hexium)
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, outcome := rt.mods.resolveForInstall(t.Context(), inst, &modInstallPayload{
		FullName: "Only-Ts", Version: "2.0.0", Source: "thunderstore",
	})
	if outcome != nil {
		t.Fatalf("Thunderstore install failed: %+v", outcome)
	}
	for _, pkg := range pkgs {
		if pkg.src != source.Thunderstore {
			t.Errorf("%s resolved to disabled %s", pkg.fullName, pkg.src)
		}
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{"full_name": "Only-Hex", "version": "1.0.0", "source": "hexium"})))
	if rec.Code != http.StatusConflict {
		t.Errorf("disabled registry: status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	rec = as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search", http.NoBody))
	var page modSearchResponse
	decodeInto(t, rec, &page)
	for _, item := range page.Items {
		if item.Source == "hexium" {
			t.Errorf("disabled registry listed: %+v", item)
		}
	}
}

func TestCatalogueFreshnessUsesSelectedRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	stamp := "2026-09-21T12:00:00Z"
	if err := db.KVSet(t.Context(), kvSyncedAt(source.Thunderstore), stamp); err != nil {
		t.Fatal(err)
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search?source=thunderstore", http.NoBody))
	var page modSearchResponse
	decodeInto(t, rec, &page)
	if page.SyncedAt == nil || *page.SyncedAt != stamp {
		t.Errorf("Thunderstore freshness = %v, want %s", page.SyncedAt, stamp)
	}
}

func TestInstalledRegistrySurvivesResolution(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			rt, db, admin, _ := world(t)
			seedBothRegistries(t, db)
			installRegistryFixture(t, db, BepInExPack, "5.4.2350", source.Hexium)
			if disabled {
				delete(rt.mods.Clients, source.Hexium)
				delete(rt.mods.Caches, source.Hexium)
			}
			rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
				jsonBody(t, map[string]string{"full_name": "Only-Ts", "version": "2.0.0"})))
			if rec.Code != http.StatusOK {
				t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
			}
			var got resolveResponse
			decodeInto(t, rec, &got)
			for _, node := range got.Nodes {
				if node.FullName == BepInExPack {
					if node.Source != "hexium" || !node.NoOp {
						t.Errorf("installed framework = %+v", node)
					}
					return
				}
			}
			t.Fatal("installed framework missing")
		})
	}
}

func TestUpdateVersionUsesInstalledRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, installed, latest string
		source                  source.Source
		want                    string
	}{
		{"upgrade", "1.0.0", "2.0.0", source.Thunderstore, "2.0.0"},
		{"same", "2.0.0", "2.0.0", source.Thunderstore, ""},
		{"older", "2.0.0", "1.0.0", source.Thunderstore, ""},
		{"other registry", "1.0.0", "2.0.0", source.Hexium, ""},
		{"stable after beta", "2.0.0-beta.1", "2.0.0", source.Thunderstore, "2.0.0"},
		{"invalid version", "1.0.0", "invalid", source.Thunderstore, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := modUpdateVersion(&store.InstanceMod{Source: source.Thunderstore, Version: tc.installed},
				&store.ModPackage{Source: tc.source, LatestVersion: tc.latest})
			if got != tc.want {
				t.Errorf("update version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRegistryStatusDescribesPartialCatalogue(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	stamp := "2026-09-21T12:00:00Z"
	if err := db.KVSet(t.Context(), kvSyncedAt(source.Thunderstore), stamp); err != nil {
		t.Fatal(err)
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search", http.NoBody))
	var page modSearchResponse
	decodeInto(t, rec, &page)
	if page.SyncedAt != nil {
		t.Errorf("partial catalogue freshness = %s, want null", *page.SyncedAt)
	}
	if len(page.Registries) != 2 {
		t.Fatalf("registry statuses = %+v", page.Registries)
	}
	for _, status := range page.Registries {
		if !status.Enabled {
			t.Errorf("registry unexpectedly disabled: %+v", status)
		}
		if status.Source == "thunderstore" && (status.SyncedAt == nil || *status.SyncedAt != stamp) {
			t.Errorf("Thunderstore status = %+v", status)
		}
		if status.Source == "hexium" && status.SyncedAt != nil {
			t.Errorf("Hexium status = %+v", status)
		}
	}
}

func TestInstalledMetadataRetainsDisabledRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	installRegistryFixture(t, db, "Only-Hex", "0.9.0", source.Hexium)
	if _, err := db.Writer.ExecContext(t.Context(), `
		UPDATE mod_packages SET is_deprecated = TRUE
		WHERE full_name = 'Only-Hex' AND source = 'hexium'`); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		if !enabled {
			delete(rt.mods.Clients, source.Hexium)
		}
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods", http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("installed: %d %s", rec.Code, rec.Body)
		}
		var body struct {
			Mods []installedModView `json:"mods"`
		}
		decodeInto(t, rec, &body)
		if len(body.Mods) != 1 {
			t.Fatalf("mods = %+v", body.Mods)
		}
		mod := body.Mods[0]
		wantUpdate := ""
		if enabled {
			wantUpdate = "1.0.0"
		}
		if mod.Source != "hexium" || mod.Name != "Hex" || !mod.IsDeprecated || mod.UpdateVersion != wantUpdate {
			t.Errorf("enabled=%v: metadata = %+v", enabled, mod)
		}
	}
}

// TestAPackagePulledFromTheIndexSaysSo is Q39's hard case. A package installed and later pulled
// upstream keeps its catalogue row, since a sync never deletes one, so only the row's stamp
// predating the registry's last complete listing tells it apart. It must read as not indexed and
// offer no update: its latest version is what the registry offered before pulling it. Before any
// complete listing the panel has nothing to say, and says nothing.
func TestAPackagePulledFromTheIndexSaysSo(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	installRegistryFixture(t, db, "Only-Ts", "1.0.0", source.Thunderstore)

	installed := func() installedModView {
		t.Helper()
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods", http.NoBody))
		var body struct {
			Mods []installedModView `json:"mods"`
		}
		decodeInto(t, rec, &body)
		if len(body.Mods) != 1 {
			t.Fatalf("mods = %+v", body.Mods)
		}
		return body.Mods[0]
	}
	listingStarted := func(at time.Time) {
		t.Helper()
		if err := db.KVSet(t.Context(), kvListingStarted(source.Thunderstore), store.FormatTime(at)); err != nil {
			t.Fatal(err)
		}
	}
	pending := func() int {
		t.Helper()
		targets, err := rt.mods.pendingUpdates(t.Context(), "inst-a")
		if err != nil {
			t.Fatal(err)
		}
		return len(targets)
	}

	if mod := installed(); mod.NotIndexed || mod.UpdateVersion != "2.0.0" {
		t.Errorf("before any complete listing: %+v, want silence and the update", mod)
	}

	listingStarted(time.Now().Add(-time.Hour))
	if mod := installed(); mod.NotIndexed || mod.UpdateVersion != "2.0.0" {
		t.Errorf("a package the last listing stamped: %+v, want it listed with its update", mod)
	}

	listingStarted(time.Now().Add(time.Hour))
	if mod := installed(); !mod.NotIndexed || mod.UpdateVersion != "" {
		t.Errorf("a package the last listing left out: %+v, want not_indexed and no update", mod)
	}
	if n := pending(); n != 0 {
		t.Errorf("Update all offers %d targets, want the pulled package left out", n)
	}

	seed(t, db, `DELETE FROM mod_packages WHERE full_name = 'Only-Ts'`)
	if mod := installed(); !mod.NotIndexed {
		t.Errorf("a package with no catalogue row after a complete listing: %+v, want not_indexed", mod)
	}
}

// TestUnlistedNeedsACompleteListingAndAnOlderStamp pins the rule's edges: a stamp equal to the
// listing's start was written by that listing, and a stamp that does not parse proves nothing.
func TestUnlistedNeedsACompleteListingAndAnOlderStamp(t *testing.T) {
	started := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	starts := map[source.Source]time.Time{source.Thunderstore: started}
	row := func(src source.Source, listedAt string, catalogued bool) *store.CataloguedMod {
		c := &store.CataloguedMod{ListedAt: listedAt}
		c.Source = src
		if catalogued {
			c.Package = &store.ModPackage{Source: src}
		}
		return c
	}
	for name, tc := range map[string]struct {
		mod  *store.CataloguedMod
		want bool
	}{
		"no complete listing":         {row(source.Hexium, "", false), false},
		"no row after a listing":      {row(source.Thunderstore, "", false), true},
		"stamped before the listing":  {row(source.Thunderstore, store.FormatTime(started.Add(-time.Second)), true), true},
		"stamped at the listing":      {row(source.Thunderstore, store.FormatTime(started), true), false},
		"stamped after the listing":   {row(source.Thunderstore, store.FormatTime(started.Add(time.Second)), true), false},
		"a stamp that does not parse": {row(source.Thunderstore, "yesterday", true), false},
	} {
		if got := unlisted(tc.mod, starts); got != tc.want {
			t.Errorf("%s: unlisted = %v, want %v", name, got, tc.want)
		}
	}
}

// TestCatalogueDetailHidesDisabledRegistry is the counterpart to
// TestInstalledMetadataRetainsDisabledRegistry: an installed package keeps its metadata when
// its registry is switched off, but the catalogue stops offering that registry's listing.
// Serving it would advertise versions and download URLs that resolveForInstall refuses.
func TestCatalogueDetailHidesDisabledRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	delete(rt.mods.Clients, source.Hexium)

	get := func(path string) int {
		return as(rt, admin, httptest.NewRequest(http.MethodGet, path, http.NoBody)).Code
	}

	if code := get("/api/v1/mods/Only/Hex?source=hexium"); code != http.StatusNotFound {
		t.Errorf("a disabled registry's package = %d, want 404", code)
	}
	if code := get("/api/v1/mods/Only/Hex"); code != http.StatusNotFound {
		t.Errorf("a disabled registry's package with no preference = %d, want 404", code)
	}
	if code := get("/api/v1/mods/Only/Ts"); code != http.StatusOK {
		t.Errorf("an enabled registry's package = %d, want 200", code)
	}

	// Carried by both, with only Thunderstore enabled: the enabled row answers rather than
	// the package reading as missing because the disabled one was picked first.
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/mods/denikson/BepInExPack_Valheim", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("package carried by both = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Source   string `json:"source"`
		Versions []struct {
			Source string `json:"source"`
		} `json:"versions"`
	}
	decodeInto(t, rec, &got)
	if got.Source != "thunderstore" {
		t.Errorf("source = %q, want thunderstore", got.Source)
	}
	for _, v := range got.Versions {
		if v.Source != "thunderstore" {
			t.Errorf("version from %q leaked into the listing", v.Source)
		}
	}
}
