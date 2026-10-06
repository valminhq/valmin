package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// grantModsManage adds the grantable mods.manage capability to the member's existing
// viewer grant on inst-a (world(t)'s own seed) — mods.manage sits in 09 §3.2's
// grantable-extras set, off by default even for an operator, so a test that wants it must
// ask for it explicitly, the same way an admin would.
func grantModsManage(t *testing.T, db *store.DB) {
	t.Helper()
	seed(
		t,
		db,
		`UPDATE instance_grants SET perms = '["mods.manage"]' WHERE user_id = 'u-member' AND instance_id = 'inst-a'`,
	)
}

func resolveBody(fullName, version string) map[string]string {
	return map[string]string{"full_name": fullName, "version": version}
}

func TestResolveRequiresModsManage(t *testing.T) {
	rt, db, admin, member := world(t)
	seedResolverIndex(t, db)

	adminRec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("ValheimModding-Jotunn", "2.29.2"))))
	if adminRec.Code != http.StatusOK {
		t.Errorf("admin: status = %d, want 200 (%s)", adminRec.Code, adminRec.Body)
	}

	memberRec := as(rt, member, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("ValheimModding-Jotunn", "2.29.2"))))
	if memberRec.Code != http.StatusForbidden {
		t.Errorf("member without mods.manage: status = %d, want 403 (%s)", memberRec.Code, memberRec.Body)
	}

	grantModsManage(t, db)
	grantedRec := as(rt, member, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("ValheimModding-Jotunn", "2.29.2"))))
	if grantedRec.Code != http.StatusOK {
		t.Errorf("member with mods.manage: status = %d, want 200 (%s)", grantedRec.Code, grantedRec.Body)
	}
}

// TestResolveInvisibleInstanceIsNotFound is D2 for this endpoint: inst-b is real, but the
// member holds no grant on it at all.
func TestResolveInvisibleInstanceIsNotFound(t *testing.T) {
	rt, _, _, member := world(t)
	rec := as(rt, member, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-b/mods/resolve",
		jsonBody(t, resolveBody("A-A", "1.0.0"))))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// seedResolverIndex writes the real corpus's own three-deep tree into mod_versions:
// OdinArchitect -> Jotunn -> the BepInEx pack.
func seedResolverIndex(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.UpsertModPackages(t.Context(), nil, []store.ModVersion{
		{
			Source:           source.Thunderstore,
			FullName:         "OdinPlus-OdinArchitect",
			Version:          "1.7.0",
			DependenciesJSON: `["ValheimModding-Jotunn-2.29.2"]`,
		},
		{
			Source:           source.Thunderstore,
			FullName:         "ValheimModding-Jotunn",
			Version:          "2.29.2",
			DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2333"]`,
		},
		{
			Source:           source.Thunderstore,
			FullName:         "denikson-BepInExPack_Valheim",
			Version:          "5.4.2333",
			DependenciesJSON: `[]`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveReturnsTheClosureAndWritesNothing(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedResolverIndex(t, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("OdinPlus-OdinArchitect", "1.7.0"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var got struct {
		Nodes []struct {
			FullName   string `json:"full_name"`
			Version    string `json:"version"`
			Transitive bool   `json:"transitive"`
		} `json:"nodes"`
	}
	decodeInto(t, rec, &got)
	if len(got.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want 3", got.Nodes)
	}
	byName := map[string]bool{}
	for _, n := range got.Nodes {
		byName[n.FullName] = n.Transitive
	}
	if transitive, ok := byName["OdinPlus-OdinArchitect"]; !ok || transitive {
		t.Error("the requested package must be present and not transitive")
	}
	if transitive, ok := byName["denikson-BepInExPack_Valheim"]; !ok || !transitive {
		t.Error("the BepInEx pack must be present and marked transitive")
	}

	var count int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM instance_mods`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("instance_mods rows = %d, want 0 (resolve is a dry run)", count)
	}
}

// TestResolveReportsFromVersionAndBackup asserts each node names the installed version it
// replaces, and backup is set only when an installed package changes version on a server
// with a world.
func TestResolveReportsFromVersionAndBackup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		installed  [][2]string
		world      bool
		request    [2]string
		wantFrom   map[string]string
		wantBackup bool
	}{
		{
			name: "fresh install", world: true, request: [2]string{"Ns-Only", "1.0.0"},
			wantFrom: map[string]string{"Ns-Only": "", manager.BepInExPack: ""},
		},
		{
			name: "update", installed: [][2]string{{"Ns-Only", "1.0.0"}}, world: true,
			request:    [2]string{"Ns-Only", "2.0.0"},
			wantFrom:   map[string]string{"Ns-Only": "1.0.0"},
			wantBackup: true,
		},
		{
			name: "update without a world", installed: [][2]string{{"Ns-Only", "1.0.0"}},
			request:  [2]string{"Ns-Only", "2.0.0"},
			wantFrom: map[string]string{"Ns-Only": "1.0.0"},
		},
		{
			name: "framework bump", installed: [][2]string{{manager.BepInExPack, "5.4.2333"}}, world: true,
			request:    [2]string{"Smoothbrain-Sailing", "1.1.8"},
			wantFrom:   map[string]string{"Smoothbrain-Sailing": "", manager.BepInExPack: "5.4.2333"},
			wantBackup: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, admin, _, dataDir := installWorld(t, append(twoVersions(), aPlainMod(),
				modPackageFixture{fullName: manager.BepInExPack, version: "5.4.2333", files: bepinexZip()},
				modPackageFixture{fullName: manager.BepInExPack, version: "5.4.2400", files: bepinexZip()},
			)...)
			for _, p := range tc.installed {
				installOK(t, rt, admin, p[0], p[1])
			}
			if tc.world {
				giveWorld(t, dataDir)
			}

			rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
				jsonBody(t, resolveBody(tc.request[0], tc.request[1]))))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
			}
			var got resolveResponse
			decodeInto(t, rec, &got)
			if got.Backup != tc.wantBackup {
				t.Errorf("backup = %v, want %v", got.Backup, tc.wantBackup)
			}
			from := map[string]string{}
			for _, n := range got.Nodes {
				from[n.FullName] = n.FromVersion
			}
			for name, want := range tc.wantFrom {
				if have, ok := from[name]; !ok || have != want {
					t.Errorf("%s from_version = %q (present %v), want %q", name, have, ok, want)
				}
			}
		})
	}
}

func TestResolveUnresolvedDependencyIs409(t *testing.T) {
	rt, db, admin, _ := world(t)
	if err := db.UpsertModPackages(
		t.Context(),
		nil,
		[]store.ModVersion{
			{
				Source:           source.Thunderstore,
				FullName:         "A-A",
				Version:          "1.0.0",
				DependenciesJSON: `["Missing-Package-9.9.9"]`,
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("A-A", "1.0.0"))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "dependency_unresolved" {
		t.Errorf("code = %q, want dependency_unresolved", got)
	}

	var body struct {
		Error struct {
			Details struct {
				Missing string `json:"missing"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &body)
	if body.Error.Details.Missing != "Missing-Package-9.9.9" {
		t.Errorf("details.missing = %q, want %q", body.Error.Details.Missing, "Missing-Package-9.9.9")
	}
}

func TestResolveCycleIs409(t *testing.T) {
	rt, db, admin, _ := world(t)
	if err := db.UpsertModPackages(t.Context(), nil, []store.ModVersion{
		{Source: source.Thunderstore, FullName: "A-A", Version: "1.0.0", DependenciesJSON: `["B-B-1.0.0"]`},
		{Source: source.Thunderstore, FullName: "B-B", Version: "1.0.0", DependenciesJSON: `["A-A-1.0.0"]`},
	}); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("A-A", "1.0.0"))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "dependency_unresolved" {
		t.Errorf("code = %q, want dependency_unresolved", got)
	}
}

// TestResolveDirtyIndexIs409NotInternal: the index is externally sourced, so a dependency
// ident that does not end in a strict version is unusable data, not a panel fault. It
// answers the same 409 as a dependency that is simply absent.
func TestResolveDirtyIndexIs409NotInternal(t *testing.T) {
	rt, db, admin, _ := world(t)
	if err := db.UpsertModPackages(
		t.Context(),
		nil,
		[]store.ModVersion{
			{Source: source.Thunderstore, FullName: "A-A", Version: "1.0.0", DependenciesJSON: `["Weird-Mod-1.0"]`},
		},
	); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("A-A", "1.0.0"))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "dependency_unresolved" {
		t.Errorf("code = %q, want dependency_unresolved", got)
	}

	var body struct {
		Error struct {
			Details struct {
				Missing string `json:"missing"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &body)
	if body.Error.Details.Missing != "Weird-Mod-1.0" {
		t.Errorf("details.missing = %q, want %q", body.Error.Details.Missing, "Weird-Mod-1.0")
	}
}

func TestResolveMissingFieldsIsValidationFailed(t *testing.T) {
	rt, _, admin, _ := world(t)
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{})))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}
