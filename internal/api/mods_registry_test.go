package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// seedBothRegistries indexes one package that both registries carry at different versions,
// and one that only Hexium does.
func seedBothRegistries(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.UpsertModPackages(t.Context(),
		[]store.ModPackage{
			{
				Source:   source.Thunderstore,
				FullName: "denikson-BepInExPack_Valheim", Namespace: "denikson",
				Name: "BepInExPack_Valheim", LatestVersion: "5.4.2202",
				Downloads: 41000000, CategoriesJSON: `["Libraries"]`,
			},
			{
				Source:   source.Hexium,
				FullName: "denikson-BepInExPack_Valheim", Namespace: "denikson",
				Name: "BepInExPack_Valheim", LatestVersion: "5.4.2350",
				Downloads: 12000, CategoriesJSON: `["Libraries"]`,
			},
			{
				Source:   source.Hexium,
				FullName: "Only-Hex", Namespace: "Only", Name: "Hex",
				LatestVersion: "1.0.0", Downloads: 5, CategoriesJSON: `["Mods"]`,
			},
			{
				Source:   source.Thunderstore,
				FullName: "Only-Ts", Namespace: "Only", Name: "Ts",
				LatestVersion: "2.0.0", Downloads: 7, CategoriesJSON: `["Mods"]`,
			},
		},
		[]store.ModVersion{
			{
				Source:   source.Thunderstore,
				FullName: "denikson-BepInExPack_Valheim", Version: "5.4.2202",
				DependenciesJSON: `[]`, DownloadURL: "https://thunderstore.io/pack.zip",
			},
			{
				Source:   source.Hexium,
				FullName: "denikson-BepInExPack_Valheim", Version: "5.4.2350",
				DependenciesJSON: `[]`, DownloadURL: "https://cdn.hexium.gg/pack.zip",
			},
			{
				// Pins a package only the other registry carries, which is the closure a
				// dependency ident cannot express a registry for.
				Source:   source.Hexium,
				FullName: "Only-Hex", Version: "1.0.0",
				DependenciesJSON: `["Only-Ts-2.0.0"]`,
				DownloadURL:      "https://cdn.hexium.gg/only.zip",
			},
			{
				Source:   source.Thunderstore,
				FullName: "Only-Ts", Version: "2.0.0",
				DependenciesJSON: `[]`, DownloadURL: "https://thunderstore.io/only-ts.zip",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

type registryRow struct {
	FullName      string `json:"full_name"`
	Source        string `json:"source"`
	LatestVersion string `json:"latest_version"`
}

// TestSearchFiltersByRegistry covers the three the UI's switch offers. A package both
// registries carry is two rows in "all", each with its own version, which is what lets an
// operator choose between them.
func TestSearchFiltersByRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)

	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"every registry", "", 4},
		{"thunderstore only", "?source=thunderstore", 2},
		{"hexium only", "?source=hexium", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := as(rt, admin,
				httptest.NewRequest(http.MethodGet, "/api/v1/mods/search"+tc.query, http.NoBody))
			var page struct {
				Items []registryRow `json:"items"`
			}
			decodeInto(t, rec, &page)
			if len(page.Items) != tc.want {
				t.Fatalf("items = %+v, want %d", page.Items, tc.want)
			}
			for _, item := range page.Items {
				if item.Source == "" {
					t.Errorf("%s came back naming no registry", item.FullName)
				}
			}
		})
	}
}

// TestSearchRejectsAnUnknownRegistry: a filter that matched nothing in silence would read
// as an empty catalogue.
func TestSearchRejectsAnUnknownRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)

	rec := as(rt, admin,
		httptest.NewRequest(http.MethodGet, "/api/v1/mods/search?source=nexus", http.NoBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

// TestSearchPagesAcrossRegistries is the keyset cursor's regression test at the handler
// level: two rows sharing a full name must each be served exactly once.
func TestSearchPagesAcrossRegistries(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)

	seen := map[string]int{}
	cursor := ""
	for range 6 {
		url := "/api/v1/mods/search?limit=1"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var page struct {
			Items      []registryRow `json:"items"`
			NextCursor *string       `json:"next_cursor"`
		}
		decodeInto(t, as(rt, admin, httptest.NewRequest(http.MethodGet, url, http.NoBody)), &page)
		for _, item := range page.Items {
			seen[item.FullName+"@"+item.Source]++
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}

	if len(seen) != 4 {
		t.Fatalf("paged %d distinct rows, want 4: %v", len(seen), seen)
	}
	for row, times := range seen {
		if times != 1 {
			t.Errorf("%s was served %d times", row, times)
		}
	}
}

// TestPackageDetailPrefersARegistryAndHonoursTheNamedOne: without the parameter the detail
// route still answers, which is what keeps every caller written before the second registry
// working; with it, a registry that does not carry the package is a miss rather than a
// silent fallback to the other one's versions.
func TestPackageDetailPrefersARegistryAndHonoursTheNamedOne(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)

	for _, tc := range []struct {
		name    string
		path    string
		status  int
		source  string
		version string
	}{
		{
			"no preference falls to thunderstore",
			"/api/v1/mods/denikson/BepInExPack_Valheim",
			http.StatusOK, "thunderstore", "5.4.2202",
		},
		{
			"hexium named",
			"/api/v1/mods/denikson/BepInExPack_Valheim?source=hexium",
			http.StatusOK, "hexium", "5.4.2350",
		},
		{
			"a hexium-only package with no preference still answers",
			"/api/v1/mods/Only/Hex",
			http.StatusOK, "hexium", "1.0.0",
		},
		{
			"a registry that does not carry it is a miss",
			"/api/v1/mods/Only/Hex?source=thunderstore",
			http.StatusNotFound, "", "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := as(rt, admin, httptest.NewRequest(http.MethodGet, tc.path, http.NoBody))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
			if tc.status != http.StatusOK {
				return
			}
			var got struct {
				registryRow
				Versions []struct {
					Version string `json:"version"`
					Source  string `json:"source"`
				} `json:"versions"`
			}
			decodeInto(t, rec, &got)
			if got.Source != tc.source || got.LatestVersion != tc.version {
				t.Errorf("row = %+v, want %s at %s", got.registryRow, tc.source, tc.version)
			}
			// The version history is that registry's alone: showing the other one's
			// versions would offer an install of bytes this row does not describe.
			for _, v := range got.Versions {
				if v.Source != tc.source {
					t.Errorf("version %s came from %s, want %s", v.Version, v.Source, tc.source)
				}
			}
		})
	}
}

// TestResolveFallsBackAcrossRegistries is ADR-213's rule under its hardest case: a
// dependency ident names no registry, so a Hexium-only package depending on a version only
// Thunderstore carries must still resolve, with each node reporting where it came from.
func TestResolveFallsBackAcrossRegistries(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{
			"full_name": "Only-Hex", "version": "1.0.0", "source": "hexium",
		})))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var got struct {
		Nodes []struct {
			FullName string `json:"full_name"`
			Source   string `json:"source"`
			Version  string `json:"version"`
		} `json:"nodes"`
	}
	decodeInto(t, rec, &got)

	where := map[string]string{}
	for _, n := range got.Nodes {
		where[n.FullName] = n.Source
	}
	if where["Only-Hex"] != "hexium" {
		t.Errorf("the requested package resolved from %q, want hexium", where["Only-Hex"])
	}
	// Only Thunderstore carries this one, so the preferred registry cannot answer for it.
	// Failing here instead would make a cross-registry closure uninstallable.
	if where["Only-Ts"] != "thunderstore" {
		t.Errorf("the pinned dependency resolved from %q, want thunderstore", where["Only-Ts"])
	}
	// The framework is auto-added at the highest version any registry calls latest
	// (ADR-186), which is Hexium's — so its node reports the registry carrying that exact
	// version, not the one the request preferred and not the one carrying an older build.
	if where["denikson-BepInExPack_Valheim"] != "hexium" {
		t.Errorf("the framework resolved from %q, want hexium (it has the higher version)",
			where["denikson-BepInExPack_Valheim"])
	}
}

// TestResolveRejectsAnUnknownRegistry: silently installing from the other registry is the
// wrong bytes, not a near miss.
func TestResolveRejectsAnUnknownRegistry(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{
			"full_name": "Only-Hex", "version": "1.0.0", "source": "nexus",
		})))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// TestLatestBepInExPicksTheHighestVersionAcrossRegistries is ADR-186 meeting ADR-213: a
// framework older than the game build crashes the server on boot, so the version decides
// and the registry does not.
func TestLatestBepInExPicksTheHighestVersionAcrossRegistries(t *testing.T) {
	rt, db, _, _ := world(t)
	seedBothRegistries(t, db)
	m := rt.mods

	for _, prefer := range []source.Source{{}, source.Thunderstore, source.Hexium} {
		got, ok, err := m.latestBepInEx(t.Context(), prefer)
		if err != nil || !ok {
			t.Fatalf("prefer %v: ok = %v, err = %v", prefer, ok, err)
		}
		// Hexium carries 5.4.2350, Thunderstore 5.4.2202, whatever the request prefers.
		if got != "5.4.2350" {
			t.Errorf("prefer %v: version = %q, want 5.4.2350", prefer, got)
		}
	}
}
