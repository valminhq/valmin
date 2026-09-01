package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// seedModIndex writes straight to mod_packages/mod_versions, standing in for a completed
// thunderstore_sync (WP-M2-02 owns the sync itself; this package only reads the index).
func seedModIndex(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.UpsertModPackages(t.Context(),
		[]store.ModPackage{
			{
				FullName: "ValheimModding-Jotunn", Namespace: "ValheimModding", Name: "Jotunn",
				Description: "A modding library", LatestVersion: "2.29.2",
				Downloads: 3000, Rating: 500, CategoriesJSON: `["Libraries"]`,
			},
			{
				FullName: "Smoothbrain-Sailing", Namespace: "Smoothbrain", Name: "Sailing",
				Description: "A sailing skill", LatestVersion: "1.1.8",
				Downloads: 200, Rating: 20, CategoriesJSON: `["Mods"]`,
			},
		},
		[]store.ModVersion{
			{
				FullName: "ValheimModding-Jotunn", Version: "2.29.2",
				DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2333"]`,
				DownloadURL:      "https://thunderstore.io/package/download/ValheimModding/Jotunn/2.29.2/",
				FileSize:         814792,
			},
			{
				FullName: "ValheimModding-Jotunn", Version: "2.29.1",
				DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2202"]`,
				DownloadURL:      "https://thunderstore.io/package/download/ValheimModding/Jotunn/2.29.1/",
				FileSize:         810000,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

// ungrantedMember returns a member seeded in world(t)'s router with no instance_grants
// row at all — the case mods.list must actually gate on.
func ungrantedMember(t *testing.T, db *store.DB) *store.User {
	t.Helper()
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-ungranted', 'noone', 'argon2id$stub', 'member', ?)`, store.Now())
	return &store.User{ID: "u-ungranted", Username: "noone", Role: store.RoleMember}
}

// TestSearchRequiresAtLeastOneGrant is the authorization this handler stands in for a
// literal Can(): mods.list sits on every grant role, so a member with zero live grants
// anywhere must be refused, while one with a grant on any instance — not necessarily one
// related to mods at all — may browse.
func TestSearchRequiresAtLeastOneGrant(t *testing.T) {
	rt, db, admin, memberWithGrant := world(t)
	nobody := ungrantedMember(t, db)
	seedModIndex(t, db)

	for _, tc := range []struct {
		name string
		user *store.User
		want int
	}{
		{"admin", admin, http.StatusOK},
		{"member with a grant on inst-a", memberWithGrant, http.StatusOK},
		{"member with no grants at all", nobody, http.StatusForbidden},
	} {
		rec := as(rt, tc.user, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search", http.NoBody))
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
		}
	}
}

func TestSearchByNameSubstring(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedModIndex(t, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search?q=jotu", http.NoBody))
	var page struct {
		Items []struct {
			FullName string `json:"full_name"`
		} `json:"items"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].FullName != "ValheimModding-Jotunn" {
		t.Errorf("items = %+v, want just Jotunn", page.Items)
	}
}

// TestSearchBeforeFirstSyncIsEmptyNotAnError proves the acceptance criterion literally:
// no index yet is an empty page and a null synced_at, never a 500.
func TestSearchBeforeFirstSyncIsEmptyNotAnError(t *testing.T) {
	rt, _, admin, _ := world(t)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Items    []any   `json:"items"`
		SyncedAt *string `json:"synced_at"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 0 {
		t.Errorf("items = %v, want empty", page.Items)
	}
	if page.SyncedAt != nil {
		t.Errorf("synced_at = %q, want null", *page.SyncedAt)
	}
}

// TestSearchPaginationRoundTrip is ADR-035 exercised over real HTTP requests, limit=1:
// following next_cursor must see every row exactly once.
func TestSearchPaginationRoundTrip(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedModIndex(t, db)

	seen := map[string]bool{}
	url := "/api/v1/mods/search?limit=1"
	for range 3 {
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, url, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
		}
		var page struct {
			Items []struct {
				FullName string `json:"full_name"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		decodeInto(t, rec, &page)
		if len(page.Items) == 0 {
			break
		}
		full := page.Items[0].FullName
		if seen[full] {
			t.Fatalf("%s returned twice", full)
		}
		seen[full] = true
		if page.NextCursor == nil {
			break
		}
		url = "/api/v1/mods/search?limit=1&cursor=" + *page.NextCursor
	}
	if len(seen) != 2 {
		t.Errorf("saw %v, want both seeded packages", seen)
	}
}

func TestPackageDetailReturnsVersionHistory(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedModIndex(t, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/ValheimModding/Jotunn", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var detail struct {
		FullName string `json:"full_name"`
		Versions []struct {
			Version      string   `json:"version"`
			Dependencies []string `json:"dependencies"`
		} `json:"versions"`
	}
	decodeInto(t, rec, &detail)
	if detail.FullName != "ValheimModding-Jotunn" {
		t.Errorf("full_name = %q", detail.FullName)
	}
	if len(detail.Versions) != 2 {
		t.Fatalf("versions = %+v, want 2", detail.Versions)
	}
}

func TestPackageDetailUnknownPackageIsNotFound(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedModIndex(t, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/Nobody/Home", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestSearchToleratesOneMalformedRow: a single row with unparseable categories — an old
// sync, a hand-edited row — degrades to an empty list for that row rather than 500ing
// every caller whose page crosses it.
func TestSearchToleratesOneMalformedRow(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedModIndex(t, db)
	if err := db.UpsertModPackages(t.Context(),
		[]store.ModPackage{{
			FullName: "Broken-Package", Namespace: "Broken", Name: "Package",
			CategoriesJSON: `not valid json`,
		}}, nil,
	); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/mods/search", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite one malformed row (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Items []struct {
			FullName   string   `json:"full_name"`
			Categories []string `json:"categories"`
		} `json:"items"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 3 {
		t.Fatalf("items = %+v, want all 3 rows including the broken one", page.Items)
	}
	for _, item := range page.Items {
		if item.FullName == "Broken-Package" && len(item.Categories) != 0 {
			t.Errorf("Broken-Package categories = %v, want empty", item.Categories)
		}
	}
}
