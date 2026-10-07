package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/sharecode"
	"github.com/valminhq/valmin/internal/store"
)

// fakeCatalogue answers dependency lookups from name-version pairs, all from one registry.
func fakeCatalogue(pins map[string][]string) dependencyLookup {
	return func(fullName, version, _ string) ([]string, string, bool, error) {
		deps, ok := pins[fullName+"-"+version]
		return deps, "thunderstore", ok, nil
	}
}

func row(name, version, installedAs, side string) store.InstanceMod {
	return store.InstanceMod{
		FullName: name, Source: source.Thunderstore, Version: version,
		InstalledAs: installedAs, Side: side, Enabled: true,
	}
}

func listedNames(mods []control.ManifestMod) []string {
	out := make([]string, 0, len(mods))
	for _, m := range mods {
		out = append(out, m.FullName+"@"+m.Version)
	}
	return out
}

// TestReduceModsLeavesOutDerivableDependencies asserts which rows a code lists, and that
// expanding them gives back every row exactly.
func TestReduceModsLeavesOutDerivableDependencies(t *testing.T) {
	catalogue := fakeCatalogue(map[string][]string{
		"Pack-Big-1.0.0":     {"A-One-1.0.0", "B-Two-2.0.0"},
		"C-Mod-1.0.0":        {"B-Two-2.1.0", "BepInEx-Pack-5.4.0"},
		"A-One-1.0.0":        {},
		"A-One-1.1.0":        {},
		"B-Two-2.0.0":        {},
		"B-Two-2.1.0":        {},
		"BepInEx-Pack-5.4.0": {},
	})
	cases := []struct {
		name string
		rows []store.InstanceMod
		want []string
	}{
		{
			"modpack members and a diamond resolve from the pins",
			[]store.InstanceMod{
				row("Pack-Big", "1.0.0", store.InstalledExplicit, "client_required"),
				row("C-Mod", "1.0.0", store.InstalledExplicit, "server_only"),
				row("A-One", "1.0.0", store.InstalledDependency, "client_required"),
				row("B-Two", "2.1.0", store.InstalledDependency, "client_required"),
				row("BepInEx-Pack", "5.4.0", store.InstalledDependency, "server_only"),
			},
			[]string{"Pack-Big@1.0.0", "C-Mod@1.0.0"},
		},
		{
			"a dependency upgraded by hand stays listed",
			[]store.InstanceMod{
				row("Pack-Big", "1.0.0", store.InstalledExplicit, ""),
				row("A-One", "1.1.0", store.InstalledDependency, ""),
				row("B-Two", "2.0.0", store.InstalledDependency, ""),
			},
			[]string{"Pack-Big@1.0.0", "A-One@1.1.0"},
		},
		{
			"a dependency tagged apart from its dependents stays listed",
			[]store.InstanceMod{
				row("Pack-Big", "1.0.0", store.InstalledExplicit, "server_only"),
				row("A-One", "1.0.0", store.InstalledDependency, "client_required"),
				row("B-Two", "2.0.0", store.InstalledDependency, "server_only"),
			},
			[]string{"Pack-Big@1.0.0", "A-One@1.0.0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listed, err := reduceMods(tc.rows, catalogue)
			if err != nil {
				t.Fatal(err)
			}
			if got := listedNames(listed); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("listed = %v, want %v", got, tc.want)
			}
			full, err := expandMods(listed, catalogue)
			if err != nil {
				t.Fatal(err)
			}
			if len(full) != len(tc.rows) {
				t.Fatalf("expanded to %v, want every row", listedNames(full))
			}
			got := map[string]control.ManifestMod{}
			for _, m := range full {
				got[m.FullName] = m
			}
			for _, r := range tc.rows {
				m := control.ManifestMod{FullName: r.FullName, Version: r.Version, Source: "thunderstore", Side: r.Side}
				if !sameMod(got[r.FullName], m) {
					t.Errorf("%s expanded to %+v, want %+v", r.FullName, got[r.FullName], m)
				}
			}
		})
	}
}

func getManifestCode(t *testing.T, rt *Server, u *store.User, id string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/instances/"+id+"/manifest/code", http.NoBody))
}

const (
	tweakedOriginal = "[General]\n# Default value: true\nEnabled = true\n# Default value: 1\nScale = 1\n" +
		"# Default value:\nWebhook =\n"
	tweakedCurrent = "[General]\n# Default value: true\nEnabled = true\n# Default value: 1\nScale = 2\n" +
		"# Default value:\nWebhook = https://example.invalid/hook\n"
)

// TestManifestCodeCarriesOnlyChangedSettings asserts the code carries the enabled mods and the
// settings changed since the panel's first write, and reports what it left out.
func TestManifestCodeCarriesOnlyChangedSettings(t *testing.T) {
	rt, db, admin, inst := manifestWorld(t)
	seed(t, db, `INSERT INTO instance_mods
		(instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at)
		VALUES (?, 'Someone-Off', '1.0.0', 'explicit', 'unknown', 0, '[]', ?)`, inst.ID, store.Now())
	writeInstanceConfig(t, inst, "Tweaked.cfg", tweakedCurrent)
	writeInstanceConfig(t, inst, "Tweaked.cfg"+modconfig.OriginalSuffix, tweakedOriginal)

	rec := getManifestCode(t, rt, admin, inst.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var view templateCodeView
	decodeInto(t, rec, &view)
	if view.Mods != 1 || view.Settings != 1 || view.SecretsLeftOut != 1 || view.DisabledLeftOut != 1 {
		t.Errorf("counts = %+v, want 1 mod, 1 setting, 1 secret and 1 disabled mod", view)
	}
	tmpl, err := sharecode.Decode(view.Code, maxManifestBytes)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := listedNames(tmpl.Mods); !reflect.DeepEqual(got, []string{"ValheimModding-Jotunn@2.29.2"}) {
		t.Errorf("mods = %v, want Jotunn alone", got)
	}
	want := []control.ManifestConfig{{File: "Tweaked.cfg", Content: "[General]\nScale = 2\n"}}
	if !reflect.DeepEqual(tmpl.Configs, want) {
		t.Errorf("configs = %+v, want %+v", tmpl.Configs, want)
	}
	if tmpl.Launch.ServerName != "Ashlands" || tmpl.Launch.MemLimitMB != 6144 {
		t.Errorf("launch = %+v, want the source's", tmpl.Launch)
	}
}

// TestManifestCodeNeedsEveryCapabilityItProjects mirrors the definition export's checks.
func TestManifestCodeNeedsEveryCapabilityItProjects(t *testing.T) {
	rt, db, _, inst := manifestWorld(t)
	member := &store.User{ID: "u-member", Username: "mel", Role: store.RoleMember}
	if rec := getManifestCode(t, rt, member, inst.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("with no grant = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	seed(t, db, `INSERT INTO instance_grants (instance_id, user_id, role, granted_at)
		VALUES (?, 'u-member', 'viewer', ?)`, inst.ID, store.Now())
	if rec := getManifestCode(t, rt, member, inst.ID); rec.Code != http.StatusForbidden {
		t.Errorf("as a viewer = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestManifestImportFromCode asserts a code previews and imports like a manifest, with its
// configs merged rather than written whole.
func TestManifestImportFromCode(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedResolvablePackage(t, db, "Someone-Thing", "1.2.3")
	code, err := sharecode.Encode(&sharecode.Template{
		Name:   "shared",
		Launch: control.ManifestLaunch{ServerName: "My Server", WorldName: "MyWorld", MemLimitMB: 4096},
		Mods:   []control.ManifestMod{{FullName: "Someone-Thing", Version: "1.2.3", Side: "server_only"}},
		Configs: []control.ManifestConfig{
			{File: "Thing.cfg", Content: "[General]\nScale = 2\n"},
			{File: rconConfigFile, Content: "[1. Rcon]\nPort = 1\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/manifest/preview",
		jsonBody(t, map[string]any{"code": code})))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview manifestPreview
	decodeInto(t, rec, &preview)
	if len(preview.Mods) != 1 || !preview.Mods[0].Available || len(preview.Configs) != 1 || len(preview.Problems) != 0 {
		t.Errorf("preview = %+v, want one available mod and Thing.cfg alone", preview)
	}

	rec = postImport(t, rt, admin, map[string]any{"code": code, "name": "from-code", "password": "hunter2"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var raw string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT o.plan FROM instance_operations o JOIN instances i ON i.id = o.instance_id
		 WHERE i.name = ?`, "from-code").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan control.OperationPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.MergeConfigs || len(plan.Configs) != 1 || plan.Sides["Someone-Thing"] != "server_only" {
		t.Errorf("plan = %+v, want merged Thing.cfg and the side tag", plan)
	}
}

// TestManifestImportRefusesABadCode asserts a request without exactly one readable definition
// is refused before anything is created.
func TestManifestImportRefusesABadCode(t *testing.T) {
	cases := map[string]map[string]any{
		"neither":        {},
		"both":           {"code": sharecode.Prefix + "AA", "manifest": map[string]any{"schema": manifestSchema}},
		"damaged":        {"code": sharecode.Prefix + "!!"},
		"not a code":     {"code": "hello"},
		"a newer format": {"code": "valmin2:AAAA"},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rt, db, admin, _ := provisionWorld(t)
			body["name"], body["password"] = "imported", "hunter2"
			if rec := postImport(t, rt, admin, body); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
			}
			if instanceCount(t, db) != 0 {
				t.Error("an instance row was created by a refused import")
			}
		})
	}
}

// TestCodeConfigsSurviveAReshare asserts imported settings merge into the files the mods
// placed, keep the copies a panel edit keeps, and come back when the new server is shared.
func TestCodeConfigsSurviveAReshare(t *testing.T) {
	rt, db, _, src := manifestWorld(t)
	writeInstanceConfig(t, src, "Tweaked.cfg", tweakedCurrent)
	writeInstanceConfig(t, src, "Tweaked.cfg"+modconfig.OriginalSuffix, tweakedOriginal)
	shared, err := rt.instances.templateCode(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := sharecode.Decode(shared.Code, maxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}

	seed(t, db, `UPDATE instances SET base_port = 2556 WHERE id = ?`, src.ID)
	dest := seedStoppedInstance(t, db, "dest")
	const shipped = "## shipped by the package\n[Other]\nKeep = yes\n"
	writeInstanceConfig(t, dest, "Shipped.cfg", shipped)
	configs := append(
		slices.Clone(tmpl.Configs),
		control.ManifestConfig{File: "Shipped.cfg", Content: "[Other]\nAdded = 1\n"},
	)
	if err := control.ApplyManifestConfigs(dest, configs, true); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(instance.ServerDir(dest.DataDir), filepath.FromSlash(instance.ConfigDir))
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if got := read("Shipped.cfg"); got != shipped+"[Other]\nAdded = 1\n" {
		t.Errorf("merged Shipped.cfg =\n%s", got)
	}
	if read("Shipped.cfg"+modconfig.OriginalSuffix) != shipped || read("Tweaked.cfg"+modconfig.OriginalSuffix) != "" {
		t.Error("the originals are not the bytes the import replaced")
	}

	// The mod's first start fills in the rest of the file around the imported setting.
	writeInstanceConfig(t, dest, "Tweaked.cfg", tweakedCurrent)
	again, err := rt.instances.templateCode(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	back, err := sharecode.Decode(again.Code, maxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	want := append(
		slices.Clone(tmpl.Configs),
		control.ManifestConfig{File: "Shipped.cfg", Content: "[Other]\nAdded = 1\n"},
	)
	if len(back.Configs) != 2 || back.Configs[0] != want[1] || back.Configs[1] != want[0] {
		t.Errorf("re-shared configs = %+v, want %+v", back.Configs, want)
	}
}
