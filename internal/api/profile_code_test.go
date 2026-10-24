package api

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/profile"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

const profileKey = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// profileHostFake is a registry's profile endpoint: it answers every request with status and
// reply, and records the body of each upload.
type profileHostFake struct {
	host    *profile.Host
	uploads []string
	gets    int
}

func newProfileHostFake(t *testing.T, status int, reply string) *profileHostFake {
	t.Helper()
	f := &profileHostFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			f.uploads = append(f.uploads, string(body))
		} else {
			f.gets++
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	f.host = &profile.Host{BaseURL: srv.URL, Client: srv.Client()}
	return f
}

// storedProfile is the text a registry stores for a profile archive with these files.
func storedProfile(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return "#r2modman\n" + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func previewCode(t *testing.T, rt *Server, u *store.User, code string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/manifest/preview",
		jsonBody(t, map[string]any{"code": code})))
}

const profileManifestText = `profileName: Friends
mods:
  - name: denikson-BepInExPack_Valheim
    version: {major: 5, minor: 4, patch: 2333}
    enabled: true
  - name: Someone-Thing
    version: {major: 1, minor: 2, patch: 3}
    enabled: true
  - name: Someone-Off
    version: {major: 1, minor: 0, patch: 0}
    enabled: false
`

// seedProfilePackages makes Someone-Thing, which depends on the framework, resolvable.
func seedProfilePackages(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.UpsertModPackages(t.Context(),
		[]store.ModPackage{
			{
				Source:        source.Thunderstore,
				FullName:      "Someone-Thing",
				Namespace:     "Someone",
				Name:          "Thing",
				LatestVersion: "1.2.3",
			},
			{
				Source:        source.Thunderstore,
				FullName:      "denikson-BepInExPack_Valheim",
				Namespace:     "denikson",
				Name:          "BepInExPack_Valheim",
				LatestVersion: "5.4.2333",
			},
		},
		[]store.ModVersion{
			{
				Source:           source.Thunderstore,
				FullName:         "Someone-Thing",
				Version:          "1.2.3",
				DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2333"]`,
			},
			{
				Source:           source.Thunderstore,
				FullName:         "denikson-BepInExPack_Valheim",
				Version:          "5.4.2333",
				DependenciesJSON: "[]",
			},
		})
	if err != nil {
		t.Fatal(err)
	}
}

// TestProfileCodeImport asserts an r2modman or Gale code previews and imports as a definition:
// enabled mods only, the mod nothing depends on first, and config files merged.
func TestProfileCodeImport(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := provisionWorld(t)
	seedProfilePackages(t, db)
	stored := storedProfile(t, map[string]string{
		"export.r2x":                  profileManifestText,
		"BepInEx/config/Thing.cfg":    "[General]\nScale = 2\n",
		"BepInEx/config/BepInEx.cfg":  "[Logging.Console]\nEnabled = false\n",
		"BepInEx/plugins/Thing.dll":   "MZ",
		"BepInEx/config/sub/Deep.cfg": "[A]\nB = 1\n",
	})
	thunderstore := newProfileHostFake(t, http.StatusNotFound, `{"detail":"Not found."}`)
	hexium := newProfileHostFake(t, http.StatusOK, stored)
	rt.instances.Profiles = profileHosts{source.Thunderstore: thunderstore.host, source.Hexium: hexium.host}

	rec := previewCode(t, rt, admin, " "+strings.ToUpper(profileKey)+"\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview manifestPreview
	decodeInto(t, rec, &preview)
	if thunderstore.gets != 1 || hexium.gets != 1 {
		t.Errorf("hosts asked %d and %d times, want once each, Thunderstore first", thunderstore.gets, hexium.gets)
	}
	names := make([]string, 0, len(preview.Mods))
	for _, m := range preview.Mods {
		names = append(names, m.FullName)
		if !m.Available {
			t.Errorf("%s is unavailable, want it in the catalogue", m.FullName)
		}
	}
	if want := "Someone-Thing,denikson-BepInExPack_Valheim"; strings.Join(names, ",") != want {
		t.Errorf("mods = %v, want %s: the disabled mod dropped and the one nothing needs first", names, want)
	}
	if preview.Name != "Friends" || preview.Instance.ServerName != "Friends" ||
		preview.Instance.WorldName != "Dedicated" {
		t.Errorf(
			"name = %q, launch = %+v, want the profile's name and the default world",
			preview.Name,
			preview.Instance,
		)
	}
	if len(preview.Configs) != 1 || preview.Configs[0].File != "Thing.cfg" ||
		len(preview.Configs[0].Settings) != 1 || preview.Configs[0].Settings[0].Key != "General.Scale" {
		t.Errorf("configs = %+v, want Thing.cfg with its one setting and not BepInEx.cfg", preview.Configs)
	}
	if len(preview.Problems) != 0 {
		t.Errorf("problems = %+v, want none", preview.Problems)
	}

	rec = postImport(t, rt, admin, map[string]any{"code": profileKey, "name": "from-profile", "password": "hunter2"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var raw string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT o.plan FROM instance_operations o JOIN instances i ON i.id = o.instance_id
		 WHERE i.name = ?`, "from-profile").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan control.OperationPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.MergeConfigs || len(plan.Configs) != 1 {
		t.Errorf("plan = %+v, want Thing.cfg merged", plan)
	}
}

// TestProfileCodeRefusals asserts what a code no registry can serve answers.
func TestProfileCodeRefusals(t *testing.T) {
	t.Parallel()
	notFound := `{"detail":"Not found."}`
	tests := []struct {
		name       string
		ts, hexium [2]any
		want       int
	}{
		{"neither registry has it", [2]any{404, notFound}, [2]any{404, notFound}, http.StatusUnprocessableEntity},
		{
			"a registry is down and the other lacks it",
			[2]any{502, ""},
			[2]any{404, notFound},
			http.StatusServiceUnavailable,
		},
		{
			"a registry answers with something else",
			[2]any{200, "<html>"},
			[2]any{404, notFound},
			http.StatusServiceUnavailable,
		},
		{
			"not a profile archive",
			[2]any{200, "#r2modman\n" + base64.StdEncoding.EncodeToString([]byte("nope"))},
			[2]any{404, notFound},
			http.StatusUnprocessableEntity,
		},
		{
			"a registry is down but the other has it",
			[2]any{502, ""},
			[2]any{200, "#r2modman\n" + base64.StdEncoding.EncodeToString([]byte("nope"))},
			http.StatusUnprocessableEntity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, _, admin, _ := provisionWorld(t)
			ts := newProfileHostFake(t, tt.ts[0].(int), tt.ts[1].(string))
			hx := newProfileHostFake(t, tt.hexium[0].(int), tt.hexium[1].(string))
			rt.instances.Profiles = profileHosts{source.Thunderstore: ts.host, source.Hexium: hx.host}

			rec := previewCode(t, rt, admin, profileKey)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestProfileCodeIsNotConfusedWithATemplateCode asserts text that is no UUID still reaches the
// template decoder and never a registry.
func TestProfileCodeIsNotConfusedWithATemplateCode(t *testing.T) {
	t.Parallel()
	rt, _, admin, _ := provisionWorld(t)
	ts := newProfileHostFake(t, http.StatusOK, "")
	rt.instances.Profiles = profileHosts{source.Thunderstore: ts.host}

	rec := previewCode(t, rt, admin, "valmin1:not-a-real-code")
	if rec.Code != http.StatusUnprocessableEntity || ts.gets != 0 {
		t.Errorf("status = %d, registry asked %d times; want 422 and none", rec.Code, ts.gets)
	}
}

func postExportCode(t *testing.T, rt *Server, u *store.User) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/inst-a/mods/export/code", http.NoBody))
}

// uploadedManifest reads export.r2x out of the profile text a registry was sent.
func uploadedManifest(t *testing.T, body string) string {
	t.Helper()
	encoded, ok := strings.CutPrefix(body, "#r2modman\n")
	if !ok {
		t.Fatalf("upload starts %q, want the profile prefix", body[:min(len(body), 20)])
	}
	archive, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return r2xIn(t, archive)
}

// TestExportCodeUploadsTheClientList asserts the code is the file export's profile, stored in
// Thunderstore when every mod is there.
func TestExportCodeUploadsTheClientList(t *testing.T) {
	t.Parallel()
	rt, _, admin := exportWorld(t)
	ts := newProfileHostFake(t, http.StatusOK, `{"key":"`+profileKey+`"}`)
	hx := newProfileHostFake(t, http.StatusOK, `{"key":"`+profileKey+`"}`)
	rt.mods.Profiles = profileHosts{source.Thunderstore: ts.host, source.Hexium: hx.host}

	rec := postExportCode(t, rt, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var view exportCodeView
	decodeInto(t, rec, &view)
	if view.Code != profileKey || view.Registry != "thunderstore" {
		t.Errorf("view = %+v, want the key and Thunderstore", view)
	}
	if len(ts.uploads) != 1 || len(hx.uploads) != 0 {
		t.Fatalf("uploads = %d and %d, want one to Thunderstore", len(ts.uploads), len(hx.uploads))
	}
	manifest := uploadedManifest(t, ts.uploads[0])
	for _, name := range []string{"OdinPlus-OdinArchitect", "ValheimModding-Jotunn", "denikson-BepInExPack_Valheim"} {
		if !strings.Contains(manifest, "name: "+name+"\n") {
			t.Errorf("manifest lacks %s:\n%s", name, manifest)
		}
	}
	if strings.Contains(manifest, "source:") {
		t.Errorf("manifest names a registry for Thunderstore mods:\n%s", manifest)
	}
}

// TestExportCodeUsesHexiumForAHexiumOnlyMod asserts a mod no Thunderstore listing carries moves
// the upload to Hexium and is marked there, so Gale installs it from Hexium.
func TestExportCodeUsesHexiumForAHexiumOnlyMod(t *testing.T) {
	t.Parallel()
	rt, db, admin := exportWorld(t)
	err := db.UpsertModPackages(
		t.Context(),
		[]store.ModPackage{
			{Source: source.Hexium, FullName: "Hex-Only", Namespace: "Hex", Name: "Only", LatestVersion: "1.0.0"},
		},
		[]store.ModVersion{{Source: source.Hexium, FullName: "Hex-Only", Version: "1.0.0", DependenciesJSON: "[]"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO instance_mods (instance_id, full_name, source, version, installed_as, side,
		enabled, file_manifest, installed_at) VALUES ('inst-a', 'Hex-Only', 'hexium', '1.0.0', 'explicit',
		'client_required', 1, '[]', ?)`, store.Now())
	ts := newProfileHostFake(t, http.StatusOK, `{"key":"`+profileKey+`"}`)
	hx := newProfileHostFake(t, http.StatusOK, `{"key":"`+profileKey+`"}`)
	rt.mods.Profiles = profileHosts{source.Thunderstore: ts.host, source.Hexium: hx.host}

	rec := postExportCode(t, rt, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var view exportCodeView
	decodeInto(t, rec, &view)
	if view.Registry != "hexium" || len(hx.uploads) != 1 || len(ts.uploads) != 0 {
		t.Fatalf(
			"registry = %q, uploads = %d and %d; want one to Hexium",
			view.Registry,
			len(ts.uploads),
			len(hx.uploads),
		)
	}
	if manifest := uploadedManifest(t, hx.uploads[0]); !strings.Contains(manifest, "name: Hex-Only\n") ||
		!strings.Contains(manifest, "source: hexium\n") {
		t.Errorf("manifest = %s, want Hex-Only marked as a Hexium mod", manifest)
	}
}

// TestExportCodeRefusals asserts a conflicting modpack is never uploaded and a registry's
// refusal reaches the caller.
func TestExportCodeRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		tag    bool
		want   int
	}{
		{"server-only dependency", 200, true, http.StatusConflict},
		{"registry says too large", http.StatusRequestEntityTooLarge, false, http.StatusRequestEntityTooLarge},
		{"registry is down", http.StatusBadGateway, false, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, _, admin := exportWorld(t)
			if tt.tag {
				if rec := patchMod(t, rt, admin, "ValheimModding-Jotunn",
					map[string]any{"side": "server_only"}); rec.Code != http.StatusOK {
					t.Fatalf("tagging status = %d, want 200 (%s)", rec.Code, rec.Body)
				}
			}
			ts := newProfileHostFake(t, tt.status, `{"key":"`+profileKey+`"}`)
			rt.mods.Profiles = profileHosts{source.Thunderstore: ts.host}

			rec := postExportCode(t, rt, admin)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
			if tt.tag && len(ts.uploads) != 0 {
				t.Errorf("uploaded %d times despite the conflict", len(ts.uploads))
			}
		})
	}
}

// seedThingInBothRegistries makes Someone-Thing 1.2.3 available from Thunderstore and from Hexium.
func seedThingInBothRegistries(t *testing.T, db *store.DB) {
	t.Helper()
	for _, src := range []source.Source{source.Thunderstore, source.Hexium} {
		err := db.UpsertModPackages(
			t.Context(),
			[]store.ModPackage{
				{Source: src, FullName: "Someone-Thing", Namespace: "Someone", Name: "Thing", LatestVersion: "1.2.3"},
			},
			[]store.ModVersion{{Source: src, FullName: "Someone-Thing", Version: "1.2.3", DependenciesJSON: "[]"}},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func singleModProfile(t *testing.T, extra string) string {
	t.Helper()
	return storedProfile(t, map[string]string{
		"export.r2x": "profileName: One\nmods:\n  - name: Someone-Thing\n    version: {major: 1, minor: 2, patch: 3}\n" +
			"    enabled: true\n" + extra,
	})
}

// TestProfileCodeFollowsTheRegistryThatServedIt asserts a mod the profile does not place comes
// from the registry that served the code when that registry carries it, and a named registry is
// kept whichever registry served the code.
func TestProfileCodeFollowsTheRegistryThatServedIt(t *testing.T) {
	t.Parallel()
	notFound := `{"detail":"Not found."}`
	tests := []struct {
		name      string
		servedBy  source.Source
		extra     string
		bothSeeds bool
		want      string
	}{
		{"served by Hexium", source.Hexium, "", true, "hexium"},
		{"served by Thunderstore", source.Thunderstore, "", true, "thunderstore"},
		{"names Hexium, served by Thunderstore", source.Thunderstore, "    source: hexium\n", true, "hexium"},
		{"origin lacks the version", source.Hexium, "", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, db, admin, _ := provisionWorld(t)
			if tt.bothSeeds {
				seedThingInBothRegistries(t, db)
			} else {
				seedProfilePackages(t, db)
			}
			stored := singleModProfile(t, tt.extra)
			hosts := profileHosts{
				source.Thunderstore: newProfileHostFake(t, http.StatusNotFound, notFound).host,
				source.Hexium:       newProfileHostFake(t, http.StatusNotFound, notFound).host,
			}
			hosts[tt.servedBy] = newProfileHostFake(t, http.StatusOK, stored).host
			rt.instances.Profiles = hosts

			rec := previewCode(t, rt, admin, profileKey)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
			}
			var preview manifestPreview
			decodeInto(t, rec, &preview)
			if len(preview.Mods) != 1 || preview.Mods[0].Source != tt.want {
				t.Errorf("mods = %+v, want one from %q", preview.Mods, tt.want)
			}
		})
	}
}

// TestProfileCodeRefusesAnUnknownRegistry asserts a registry name this panel does not know is
// reported, not dropped.
func TestProfileCodeRefusesAnUnknownRegistry(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := provisionWorld(t)
	seedThingInBothRegistries(t, db)
	rt.instances.Profiles = profileHosts{
		source.Thunderstore: newProfileHostFake(t, http.StatusOK, singleModProfile(t, "    source: elsewhere\n")).host,
	}

	rec := previewCode(t, rt, admin, profileKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview manifestPreview
	decodeInto(t, rec, &preview)
	if len(preview.Problems) != 1 || !strings.Contains(preview.Problems[0].Detail, "elsewhere") {
		t.Errorf("problems = %+v, want the unknown registry named", preview.Problems)
	}
}
