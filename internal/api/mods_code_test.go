package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

func postCode(t *testing.T, rt *Server, u *store.User, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost,
		"/api/v1/instances/inst-a/mods/code/"+path, jsonBody(t, body)))
}

func previewOfCode(t *testing.T, rt *Server, u *store.User, body map[string]any) codePreview {
	t.Helper()
	rec := postCode(t, rt, u, "preview", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview codePreview
	decodeInto(t, rec, &preview)
	return preview
}

// serveCode makes the mod's profile available from the registry, as the one that served the code.
func serveCode(t *testing.T, rt *Server, served source.Source, manifest string) {
	t.Helper()
	stored := storedProfile(t, map[string]string{"export.r2x": manifest})
	notFound := `{"detail":"Not found."}`
	hosts := profileHosts{
		source.Thunderstore: newProfileHostFake(t, http.StatusNotFound, notFound).host,
		source.Hexium:       newProfileHostFake(t, http.StatusNotFound, notFound).host,
	}
	hosts[served] = newProfileHostFake(t, http.StatusOK, stored).host
	rt.mods.Profiles = hosts
}

// profileManifestOf is an export.r2x listing the mods, each as name, version and optional source.
func profileManifestOf(mods ...[3]string) string {
	var b strings.Builder
	b.WriteString("profileName: Friends\nmods:\n")
	for _, m := range mods {
		parts := strings.Split(m[1], ".")
		b.WriteString("  - name: " + m[0] + "\n    version: {major: " + parts[0] + ", minor: " + parts[1] +
			", patch: " + parts[2] + "}\n    enabled: true\n")
		if m[2] != "" {
			b.WriteString("    source: " + m[2] + "\n")
		}
	}
	return b.String()
}

func installedVersions(t *testing.T, db *store.DB) map[string][2]string {
	t.Helper()
	rows, err := db.InstanceMods(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for i := range rows {
		out[rows[i].FullName] = [2]string{rows[i].Version, rows[i].Source.String()}
	}
	return out
}

// codeApplyWorld installs the old versions of the update fixtures, plus a mod that can be
// lowered, and offers one new mod from Thunderstore and from Hexium.
func codeApplyWorld(t *testing.T) (*Server, *store.DB, *store.User) {
	t.Helper()
	fixtures := append(
		updatable(),
		modPackageFixture{
			fullName: "Ns-Down",
			version:  "1.0.0",
			files:    map[string]string{"manifest.json": "{}", "plugins/Down.dll": "down 1"},
		},
		modPackageFixture{
			fullName: "Ns-Down",
			version:  "2.0.0",
			files:    map[string]string{"manifest.json": "{}", "plugins/Down.dll": "down 2"},
		},
		modPackageFixture{
			fullName: "Ns-Fresh",
			version:  "1.0.0",
			files:    map[string]string{"manifest.json": "{}", "plugins/Fresh.dll": "fresh"},
		},
	)
	rt, db, admin, _, dataDir := installWorld(t, fixtures...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "Ns-Only", "1.0.0")
	installClosure(t, rt, admin, "Ns-Other", "1.0.0")
	installClosure(t, rt, admin, "Ns-Down", "2.0.0")
	giveWorld(t, dataDir)

	// Ns-Fresh is also in Hexium, served from the same place.
	var url string
	var size int64
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT download_url, file_size FROM mod_versions
		WHERE full_name = 'Ns-Fresh' AND version = '1.0.0' AND source = 'thunderstore'`).Scan(&url, &size); err != nil {
		t.Fatal(err)
	}
	err := db.UpsertModPackages(
		t.Context(),
		[]store.ModPackage{
			{
				Source:         source.Hexium,
				FullName:       "Ns-Fresh",
				Namespace:      "ns",
				Name:           "Ns-Fresh",
				LatestVersion:  "1.0.0",
				CategoriesJSON: "[]",
			},
		},
		[]store.ModVersion{
			{
				Source:           source.Hexium,
				FullName:         "Ns-Fresh",
				Version:          "1.0.0",
				DependenciesJSON: "[]",
				DownloadURL:      url,
				FileSize:         size,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return rt, db, admin
}

// TestApplyCodeChangesEverythingInOneJob asserts a code installs, raises and lowers mods in one
// job, each from the registry it names or that served the code, and leaves the rest alone.
func TestApplyCodeChangesEverythingInOneJob(t *testing.T) {
	t.Parallel()
	rt, db, admin := codeApplyWorld(t)
	serveCode(t, rt, source.Thunderstore, profileManifestOf(
		[3]string{"Ns-Only", "2.0.0", ""},
		[3]string{"Ns-Down", "1.0.0", ""},
		[3]string{"Ns-Fresh", "1.0.0", "hexium"},
		[3]string{"Ns-New", "1.0.0", ""},
		[3]string{"Ns-Other", "1.0.0", ""},
	))

	preview := previewOfCode(t, rt, admin, map[string]any{"code": profileKey})
	selected := map[string]bool{}
	for _, row := range preview.Rows {
		selected[row.FullName] = row.Selected
	}
	if !selected["Ns-Only"] || !selected["Ns-Fresh"] || !selected["Ns-New"] || selected["Ns-Down"] {
		t.Errorf("selected = %v, want installs and raises chosen and the lowered Ns-Down not", selected)
	}
	if preview.Matching != 1 || preview.Registry != "thunderstore" {
		t.Errorf(
			"matching = %d, registry = %q, want Ns-Other alone and Thunderstore",
			preview.Matching,
			preview.Registry,
		)
	}

	preview = previewOfCode(t, rt, admin, map[string]any{
		"code": profileKey, "select": []string{"Ns-Only", "Ns-Down", "Ns-Fresh", "Ns-New"},
	})
	if len(preview.Targets) != 4 || len(preview.Problems) != 0 || len(preview.Conflicts) != 0 {
		t.Fatalf("preview = %+v, want four targets and nothing in the way", preview)
	}

	rec := postCode(t, rt, admin, "apply", map[string]any{"targets": preview.Targets, "registry": preview.Registry})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apply status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("job = %+v, want succeeded", got)
	}

	got := installedVersions(t, db)
	want := map[string][2]string{
		"Ns-Only":  {"2.0.0", "thunderstore"},
		"Ns-Down":  {"1.0.0", "thunderstore"},
		"Ns-Fresh": {"1.0.0", "hexium"},
		"Ns-New":   {"1.0.0", "thunderstore"},
		"Ns-Other": {"1.0.0", "thunderstore"},
		"Ns-Lib":   {"1.0.0", "thunderstore"},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %v, want %v", name, got[name], w)
		}
	}
}

// TestApplyCodeNeverMovesAnInstalledModBetweenRegistries asserts an installed Hexium mod stays
// on Hexium: a code from Thunderstore that asks for a version only Thunderstore has leaves it
// alone and says why.
func TestApplyCodeNeverMovesAnInstalledModBetweenRegistries(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)
	seed(t, db, `UPDATE instances SET modded = TRUE, bepinex_version = '5.4.2333' WHERE id = 'inst-a'`)
	err := db.UpsertModPackages(t.Context(),
		[]store.ModPackage{
			{
				Source:         source.Hexium,
				FullName:       "Ns-Hex",
				Namespace:      "ns",
				Name:           "Hex",
				LatestVersion:  "1.0.0",
				CategoriesJSON: "[]",
			},
			{
				Source:         source.Thunderstore,
				FullName:       "Ns-Hex",
				Namespace:      "ns",
				Name:           "Hex",
				LatestVersion:  "2.0.0",
				CategoriesJSON: "[]",
			},
		},
		[]store.ModVersion{
			{Source: source.Hexium, FullName: "Ns-Hex", Version: "1.0.0", DependenciesJSON: "[]"},
			{Source: source.Thunderstore, FullName: "Ns-Hex", Version: "2.0.0", DependenciesJSON: "[]"},
		})
	if err != nil {
		t.Fatal(err)
	}
	installRegistryFixture(t, db, "Ns-Hex", "1.0.0", source.Hexium)

	tests := []struct {
		name  string
		named string
		skip  string
	}{
		{"code names no registry", "", manager.SkipNotInRegistry},
		{"code names Thunderstore", "thunderstore", manager.SkipOtherRegistry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveCode(t, rt, source.Thunderstore, profileManifestOf([3]string{"Ns-Hex", "2.0.0", tt.named}))

			preview := previewOfCode(t, rt, admin, map[string]any{"code": profileKey})
			if len(preview.Rows) != 1 || preview.Rows[0].Skip != tt.skip || preview.Rows[0].Source != "hexium" ||
				preview.Rows[0].Selected || len(preview.Targets) != 0 {
				t.Errorf(
					"rows = %+v, targets = %+v, want Ns-Hex skipped as %q on hexium",
					preview.Rows,
					preview.Targets,
					tt.skip,
				)
			}
		})
	}
	if got := installedVersions(t, db)["Ns-Hex"]; got != [2]string{"1.0.0", "hexium"} {
		t.Errorf("Ns-Hex = %v, want it unchanged", got)
	}
}

// TestPreviewCodeNeedsAFrameworkOnAnUnmoddedServer asserts a code of mods alone cannot be
// applied to a server that has no loader yet.
func TestPreviewCodeNeedsAFrameworkOnAnUnmoddedServer(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)
	seedThingInBothRegistries(t, db)
	seed(t, db, `UPDATE instances SET modded = FALSE WHERE id = 'inst-a'`)
	serveCode(t, rt, source.Thunderstore, profileManifestOf([3]string{"Someone-Thing", "1.2.3", ""}))

	preview := previewOfCode(t, rt, admin, map[string]any{"code": profileKey})
	if len(preview.Problems) == 0 || !strings.Contains(preview.Problems[0], "framework") {
		t.Errorf("problems = %v, want the missing framework named", preview.Problems)
	}
}

// TestPreviewCodeRefusals asserts what a code that cannot be read answers.
func TestPreviewCodeRefusals(t *testing.T) {
	t.Parallel()
	rt, _, admin, member := world(t)
	serveCode(t, rt, source.Thunderstore, profileManifestOf([3]string{"Ns-Only", "1.0.0", ""}))

	tests := []struct {
		name string
		who  *store.User
		code string
		want int
	}{
		{"not a profile code", admin, "valmin1:abc", http.StatusUnprocessableEntity},
		{"no code", admin, "", http.StatusUnprocessableEntity},
		{"a member without mods.manage", member, profileKey, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postCode(t, rt, tt.who, "preview", map[string]any{"code": tt.code})
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestApplyCodeRefusals asserts the gate on the confirmed targets.
func TestApplyCodeRefusals(t *testing.T) {
	t.Parallel()
	rt, db, admin := codeApplyWorld(t)
	if rec := patchMod(t, rt, admin, "Ns-Down", map[string]any{"locked": true}); rec.Code != http.StatusOK {
		t.Fatalf("lock status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	tests := []struct {
		name    string
		targets []manager.UpdateTarget
		running bool
		want    int
	}{
		{"nothing to apply", nil, false, http.StatusUnprocessableEntity},
		{
			"a locked mod",
			[]manager.UpdateTarget{{FullName: "Ns-Down", Source: "thunderstore", Version: "1.0.0"}},
			false,
			http.StatusUnprocessableEntity,
		},
		{
			"an installed mod from another registry",
			[]manager.UpdateTarget{{FullName: "Ns-Only", Source: "hexium", Version: "2.0.0"}},
			false,
			http.StatusUnprocessableEntity,
		},
		{
			"no registry named",
			[]manager.UpdateTarget{{FullName: "Ns-New", Version: "1.0.0"}},
			false,
			http.StatusUnprocessableEntity,
		},
		{
			"a running server",
			[]manager.UpdateTarget{{FullName: "Ns-New", Source: "thunderstore", Version: "1.0.0"}},
			true,
			http.StatusConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.running {
				seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
			}
			rec := postCode(t, rt, admin, "apply", map[string]any{"targets": tt.targets, "registry": "thunderstore"})
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestApplyCodeNeedsAFrameworkOnAnUnmoddedServer asserts the apply refuses what the preview
// warns about, since a call can skip the preview.
func TestApplyCodeNeedsAFrameworkOnAnUnmoddedServer(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, _ := installWorld(t)
	seedThingInBothRegistries(t, db)
	seed(t, db, `UPDATE instances SET modded = FALSE WHERE id = 'inst-a'`)

	rec := postCode(t, rt, admin, "apply", map[string]any{
		"targets":  []manager.UpdateTarget{{FullName: "Someone-Thing", Source: "thunderstore", Version: "1.2.3"}},
		"registry": "thunderstore",
	})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "framework") {
		t.Errorf("status = %d (%s), want 422 naming the framework", rec.Code, rec.Body)
	}
}
