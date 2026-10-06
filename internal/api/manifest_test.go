package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

// aConfigFile is deliberately not a tidy key/value list: a comment, a value nobody's schema
// knows about, and a blank line are exactly what a round trip is easiest to lose (ADR-010).
const aConfigFile = `## Settings file was created by plugin Thing v1.2.0
## Plugin GUID: com.author.thing

[General]

## Whether the mod is active.
# Setting type: Boolean
Enabled = true

WhateverNobodyDeclared = 7
`

// rconConfigFile is the ValheimRcon plugin's config file, which holds the instance's RCON
// password.
const rconConfigFile = "org.tristan.rcon.cfg"

func writeInstanceConfig(t *testing.T, inst *store.Instance, name, content string) {
	t.Helper()
	dir := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(configDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getManifest(t *testing.T, rt *Server, u *store.User, id string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/"+id+"/manifest", http.NoBody))
}

func postImport(t *testing.T, rt *Server, u *store.User, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/import", jsonBody(t, body)))
}

// manifestWorld is one provisioned-looking instance with a mod row, a config file, and every
// launch field set to something distinguishable from a default.
func manifestWorld(t *testing.T) (*Server, *store.DB, *store.User, *store.Instance) {
	t.Helper()
	rt, db, admin, _ := provisionWorld(t)
	inst := seedStoppedInstance(t, db, "source")
	seed(t, db, `UPDATE instances SET server_name = 'Ashlands', world_name = 'ashlands',
		public = 1, crossplay = 1, preset = 'hard', modifiers = ?, extra_args = '-foo',
		mem_limit_mb = 6144, cpu_limit = 2.0, backup_keep_cold = 5, backup_keep_hot = 3,
		backup_on_restart = 1, game_build_id = '21981590' WHERE id = ?`,
		`{"combat":"hard"}`, inst.ID)
	seed(t, db, `INSERT INTO instance_mods
		(instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at)
		VALUES (?, 'ValheimModding-Jotunn', '2.29.2', 'explicit', 'client_required', 1, '[]', ?)`,
		inst.ID, store.Now())
	writeInstanceConfig(t, inst, "Thing.cfg", aConfigFile)

	reloaded, err := db.InstanceByID(t.Context(), inst.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload seeded instance: %v", err)
	}
	return rt, db, admin, reloaded
}

// TestManifestExportCarriesTheDefinition is 01 G6: launch fields, pinned mods and config
// bytes, with the comments and the undeclared key intact.
func TestManifestExportCarriesTheDefinition(t *testing.T) {
	rt, _, admin, inst := manifestWorld(t)

	rec := getManifest(t, rt, admin, inst.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var m instanceManifest
	decodeInto(t, rec, &m)

	if m.Schema != manifestSchema {
		t.Errorf("schema = %d, want %d", m.Schema, manifestSchema)
	}
	want := manifestLaunch{
		ServerName: "Ashlands", WorldName: "ashlands", Public: true, Crossplay: true,
		Preset: "hard", Modifiers: map[string]string{"combat": "hard"}, ExtraArgs: "-foo",
		MemLimitMB: 6144, BackupKeepCold: 5, BackupKeepHot: 3, BackupOnRestart: true,
	}
	got := m.Instance
	cpu := got.CPULimit
	got.CPULimit = nil
	if cpu == nil || *cpu != 2.0 {
		t.Errorf("cpu_limit = %v, want 2.0", cpu)
	}
	if !jsonEqual(t, got, want) {
		t.Errorf("launch fields = %+v, want %+v", got, want)
	}
	if len(m.Mods) != 1 || m.Mods[0].FullName != "ValheimModding-Jotunn" ||
		m.Mods[0].Version != "2.29.2" || m.Mods[0].Side != "client_required" {
		t.Errorf("mods = %+v, want Jotunn pinned at 2.29.2 with its side tag", m.Mods)
	}
	if len(m.Configs) != 1 || m.Configs[0].File != "Thing.cfg" {
		t.Fatalf("configs = %+v, want the one .cfg", m.Configs)
	}
	if m.Configs[0].Content != aConfigFile {
		t.Errorf("config bytes changed in the export:\n%s", m.Configs[0].Content)
	}
}

// TestManifestExportCarriesNoIdentityAndNoSecret is the other half of ADR-151: what the
// manifest must never carry, asserted against the raw document rather than the struct, since
// a field added later would be invisible to a typed decode. The RCON plugin's config file
// holds the instance's RCON password, so it is left out whole.
func TestManifestExportCarriesNoIdentityAndNoSecret(t *testing.T) {
	rt, _, admin, inst := manifestWorld(t)
	const rconPassword = "rcon-password-of-the-source"
	writeInstanceConfig(t, inst, rconConfigFile,
		"[1. Rcon]\nPort = 2455\nPassword = "+rconPassword+"\n")

	rec := getManifest(t, rt, admin, inst.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{
		inst.ID, inst.DataDir, inst.CrossplayInstanceID,
		"base_port", "container_id", "data_dir", "password", "game_build_id", "state",
		"crossplay_instance_id", "grants", "schedules", rconConfigFile, rconPassword,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the manifest carries %q:\n%s", forbidden, body)
		}
	}

	// A second read of an unchanged instance is the same document. G6's export is a
	// projection of state, so anything that varies between two reads would be noise a diff
	// of two manifests could not see past.
	again := getManifest(t, rt, admin, inst.ID)
	if again.Body.String() != body {
		t.Error("two exports of an unchanged instance differ")
	}
}

// TestManifestExportNeedsEveryCapabilityItProjects: the document is launch settings plus mods
// plus config, so a caller who could not read one of those three cannot read it here either.
func TestManifestExportNeedsEveryCapabilityItProjects(t *testing.T) {
	rt, db, _, inst := manifestWorld(t)
	member := &store.User{ID: "u-member", Username: "mel", Role: store.RoleMember}

	// No grant at all: the instance is invisible, not forbidden (ADR-038).
	if rec := getManifest(t, rt, member, inst.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("with no grant = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	// A viewer can see the instance and read config, but holds no instance.settings.
	seed(t, db, `INSERT INTO instance_grants (instance_id, user_id, role, granted_at)
		VALUES (?, 'u-member', 'viewer', ?)`, inst.ID, store.Now())
	if rec := getManifest(t, rt, member, inst.ID); rec.Code != http.StatusForbidden {
		t.Errorf("as a viewer = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestManifestImportRefusesBeforeItWrites covers ADR-151's bounds and its filename rule. Each
// case must be refused with nothing created — validation that runs after the row is not
// validation, and an unsafe path that reaches the job is a write outside the config directory.
func TestManifestImportRefusesBeforeItWrites(t *testing.T) {
	oversized := strings.Repeat("x", maxManifestConfigSize+1)
	tooManyMods := make([]map[string]any, maxManifestMods+1)
	for i := range tooManyMods {
		tooManyMods[i] = map[string]any{"full_name": "Ns-Mod", "version": "1.0.0"}
	}

	cases := []struct {
		name     string
		manifest map[string]any
	}{
		{"an unknown schema", map[string]any{"schema": 2}},
		{"a schema of zero", map[string]any{"schema": 0}},
		{"a config path that escapes", map[string]any{
			"schema":  manifestSchema,
			"configs": []map[string]any{{"file": "../../evil.cfg", "content": "x"}},
		}},
		{"a config path with a separator", map[string]any{
			"schema":  manifestSchema,
			"configs": []map[string]any{{"file": "nested/Thing.cfg", "content": "x"}},
		}},
		{"a config that is not a .cfg", map[string]any{
			"schema":  manifestSchema,
			"configs": []map[string]any{{"file": "authorized_keys", "content": "x"}},
		}},
		{"an oversized config", map[string]any{
			"schema":  manifestSchema,
			"configs": []map[string]any{{"file": "Big.cfg", "content": oversized}},
		}},
		{"the same config twice", map[string]any{
			"schema": manifestSchema,
			"configs": []map[string]any{
				{"file": "Thing.cfg", "content": "a"}, {"file": "Thing.cfg", "content": "b"},
			},
		}},
		{"more mods than the bound", map[string]any{"schema": manifestSchema, "mods": tooManyMods}},
		{"a mod with no version", map[string]any{
			"schema": manifestSchema,
			"mods":   []map[string]any{{"full_name": "Ns-Mod"}},
		}},
		{"a mod with a side no tag has", map[string]any{
			"schema": manifestSchema,
			"mods":   []map[string]any{{"full_name": "Ns-Mod", "version": "1.0.0", "side": "both"}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, admin, _ := provisionWorld(t)
			tc.manifest["instance"] = map[string]any{
				"server_name": "My Server", "world_name": "MyWorld", "mem_limit_mb": 4096,
			}
			rec := postImport(t, rt, admin, map[string]any{
				"manifest": tc.manifest, "name": "imported", "password": "hunter2",
			})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
			}
			if instanceCount(t, db) != 0 {
				t.Error("an instance row was created by a refused import")
			}
		})
	}
}

// TestManifestImportRefusesAPinnedVersionThatIsGone: a manifest names exact versions, so a
// package the catalogue can no longer supply fails the import. Resolving to a nearby version
// would produce a server that is not the one the manifest describes (ADR-151).
func TestManifestImportRefusesAPinnedVersionThatIsGone(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedResolvablePackage(t, db, "Someone-Thing", "1.2.3")

	rec := postImport(t, rt, admin, map[string]any{
		"name": "imported", "password": "hunter2",
		"manifest": map[string]any{
			"schema": manifestSchema,
			"instance": map[string]any{
				"server_name": "My Server", "world_name": "MyWorld", "mem_limit_mb": 4096,
			},
			"mods": []map[string]any{{"full_name": "Someone-Thing", "version": "9.9.9"}},
		},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if instanceCount(t, db) != 0 {
		t.Error("an instance row was created for a manifest whose mods cannot be installed")
	}
}

// TestManifestImportProvisionsAFreshIdentity is ADR-151's identity rule: the destination is a
// new instance in every respect the source could have collided on.
func TestManifestImportProvisionsAFreshIdentity(t *testing.T) {
	rt, db, admin, source := manifestWorld(t)

	rec := getManifest(t, rt, admin, source.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d (%s)", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	// The source's one mod is not in this panel's catalogue, and an unavailable pin is its
	// own test above.
	m["mods"] = []any{}

	imported := postImport(t, rt, admin, map[string]any{
		"manifest": m, "name": "destination", "password": "hunter2",
	})
	if imported.Code != http.StatusAccepted {
		t.Fatalf("import status = %d, want 202 (%s)", imported.Code, imported.Body)
	}

	dest := instanceNamed(t, db, "destination")
	if dest.ID == source.ID || dest.CrossplayInstanceID == source.CrossplayInstanceID {
		t.Error("the import reused the source's identity")
	}
	if dest.BasePort == source.BasePort {
		t.Error("the import reused the source's port")
	}
	if dest.DataDir == source.DataDir {
		t.Error("the import reused the source's data directory")
	}
	if dest.ServerName != source.ServerName || dest.WorldName != source.WorldName ||
		dest.MemLimitMB != source.MemLimitMB || dest.Public != source.Public {
		t.Errorf("launch fields did not survive the round trip: %+v", dest)
	}
	if !sameOptionalString(dest.ExtraArgs, source.ExtraArgs) {
		t.Errorf("extra_args = %q, want %q", deref(dest.ExtraArgs), deref(source.ExtraArgs))
	}
	// The source is untouched: an import creates, it never edits something already here.
	after, err := db.InstanceByID(t.Context(), source.ID)
	if err != nil || after == nil {
		t.Fatalf("reload source: %v", err)
	}
	if after.Name != source.Name || after.BasePort != source.BasePort || after.State != source.State {
		t.Errorf("the import altered the source instance: %+v", after)
	}
}

// TestManifestImportCarriesSideTagsAndNotTheRCONConfig asserts what an import hands its
// definition chain: every mod's side tag, and every config file except the RCON plugin's,
// whose password belongs to the server the file came from. The preview lists the same files.
func TestManifestImportCarriesSideTagsAndNotTheRCONConfig(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedResolvablePackage(t, db, "Someone-Tagged", "2.0.0")
	const sourcePassword = "rcon-password-of-the-source"
	manifest := map[string]any{
		"schema": manifestSchema,
		"instance": map[string]any{
			"server_name": "My Server", "world_name": "MyWorld", "mem_limit_mb": 4096,
		},
		"mods": []map[string]any{
			{"full_name": "Someone-Tagged", "version": "2.0.0", "side": "client_required"},
		},
		"configs": []map[string]any{
			{"file": "Thing.cfg", "content": aConfigFile},
			{"file": rconConfigFile, "content": "[1. Rcon]\nPort = 2455\nPassword = " + sourcePassword + "\n"},
		},
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/manifest/preview",
		jsonBody(t, map[string]any{"manifest": manifest})))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview manifestPreview
	decodeInto(t, rec, &preview)
	if len(preview.Configs) != 1 || preview.Configs[0].File != "Thing.cfg" {
		t.Errorf("preview configs = %+v, want Thing.cfg alone", preview.Configs)
	}

	rec = postImport(t, rt, admin, map[string]any{
		"manifest": manifest, "name": "imported", "password": "hunter2",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var raw string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT o.plan FROM instance_operations o JOIN instances i ON i.id = o.instance_id
		 WHERE i.name = ?`, "imported").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, sourcePassword) {
		t.Errorf("the definition chain carries the source's RCON password: %s", raw)
	}
	var plan opPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Configs) != 1 || plan.Configs[0].File != "Thing.cfg" {
		t.Errorf("plan configs = %+v, want Thing.cfg alone", plan.Configs)
	}
	if plan.Sides["Someone-Tagged"] != "client_required" {
		t.Errorf("plan sides = %v, want Someone-Tagged tagged client_required", plan.Sides)
	}
}

// TestManifestSideTagsLandWithEachInstall asserts that an imported definition's side tags are
// on every row an install step has landed, including when a later install is refused and the
// chain stops short.
func TestManifestSideTagsLandWithEachInstall(t *testing.T) {
	cases := []struct {
		name   string
		failOn string
		want   map[string]string
	}{
		{
			"every install lands", "",
			map[string]string{"A-One": "client_required", "B-Two": "server_only"},
		},
		{
			"a later install is refused", "B-Two",
			map[string]string{"A-One": "client_required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, _, _ := provisionWorld(t)
			h := rt.instances
			inst := seedStoppedInstance(t, db, "chain-sides")
			h.Mods = &fakeModEngine{t: t, h: h, db: db, failOn: tc.failOn, onInstall: func(req resolveRequest) {
				seed(t, db, `INSERT INTO instance_mods
					(instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at)
					VALUES (?, ?, ?, 'explicit', 'unknown', 1, '[]', ?)`,
					inst.ID, req.FullName, req.Version, store.Now())
			}}

			seedChain(t, h, db, inst.ID, &opPlan{
				Mods: []manager.PackageRequest{
					{FullName: "A-One", Version: "1.0.0"}, {FullName: "B-Two", Version: "2.0.0"},
				},
				Sides: map[string]string{"A-One": "client_required", "B-Two": "server_only"},
			})
			h.operationService().Advance(t.Context(), inst.ID)

			mods, err := db.InstanceMods(t.Context(), inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, m := range mods {
				got[m.FullName] = m.Side
			}
			if !jsonEqual(t, got, tc.want) {
				t.Errorf("sides = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestManifestPreviewWritesNothing: the preview exists so an operator reads what a file would
// do — including which pins are gone — before any of it happens.
func TestManifestPreviewWritesNothing(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedResolvablePackage(t, db, "Someone-Thing", "1.2.3")

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/manifest/preview", jsonBody(t, map[string]any{
			"manifest": map[string]any{
				"schema": manifestSchema,
				"name":   "source",
				"instance": map[string]any{
					"server_name": "My Server", "world_name": "MyWorld", "mem_limit_mb": 4096,
				},
				"mods": []map[string]any{
					{"full_name": "Someone-Thing", "version": "1.2.3"},
					{"full_name": "Someone-Gone", "version": "0.0.1"},
				},
				"configs": []map[string]any{{"file": "Thing.cfg", "content": aConfigFile}},
			},
		})))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview manifestPreview
	decodeInto(t, rec, &preview)

	if len(preview.Mods) != 2 || !preview.Mods[0].Available || preview.Mods[1].Available {
		t.Errorf("mods = %+v, want the first available and the second not", preview.Mods)
	}
	if len(preview.Problems) != 1 || !strings.Contains(preview.Problems[0].Detail, "Someone-Gone") {
		t.Errorf("problems = %+v, want the missing pin named", preview.Problems)
	}
	if len(preview.Configs) != 1 || preview.Configs[0].Bytes != len(aConfigFile) {
		t.Errorf("configs = %+v, want the one file with its size", preview.Configs)
	}
	if instanceCount(t, db) != 0 {
		t.Error("the preview created an instance")
	}
}

// TestManifestConfigIsWrittenAfterTheModsAreIn is ADR-151's ordering: the mod installs place a
// package's own defaults, and the manifest's bytes are what the operator recorded, so the
// manifest goes last or the install overwrites it.
func TestManifestConfigIsWrittenAfterTheModsAreIn(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.instances
	h.Mods = &fakeModEngine{t: t, h: h, db: db}

	inst := seedStoppedInstance(t, db, "chain-config")
	writeInstanceConfig(t, inst, "Thing.cfg", "what the mod install placed")
	seedChain(t, h, db, inst.ID, &opPlan{
		Mods:    []manager.PackageRequest{{FullName: "A-One", Version: "1.0.0"}},
		Configs: []manifestConfig{{File: "Thing.cfg", Content: aConfigFile}},
	})
	h.operationService().Advance(t.Context(), inst.ID)
	waitForChain(t, db, inst.ID)

	path := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(configDir), "Thing.cfg")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != aConfigFile {
		t.Errorf("config on disk is not the manifest's:\n%s", got)
	}
}

// waitForChain blocks until the instance has no outstanding definition operation, which the
// chain's job-backed steps only reach once their runners have finished.
func waitForChain(t *testing.T, db *store.DB, instanceID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		op, err := db.OpenOperation(t.Context(), instanceID)
		if err != nil {
			t.Fatal(err)
		}
		if op == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("definition operation stalled at step %d", op.Cursor)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestManifestConfigCannotEscapeTheConfigDirectory re-checks the filename inside the job. The
// request validated it, but the payload is read back from the database long afterwards, and a
// path that escapes here writes anywhere the daemon can reach.
func TestManifestConfigCannotEscapeTheConfigDirectory(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	inst := seedStoppedInstance(t, db, "chain-escape")

	err := control.ApplyManifestConfigs(inst, []manifestConfig{{File: "../../escaped.cfg", Content: "x"}})
	if err == nil {
		t.Fatal("an escaping path was accepted")
	}
	if _, statErr := os.Stat(filepath.Join(inst.DataDir, "escaped.cfg")); statErr == nil {
		t.Error("the escaping write landed on disk")
	}
	_ = rt
}

func instanceCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func instanceNamed(t *testing.T, db *store.DB, name string) *store.Instance {
	t.Helper()
	var id string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT id FROM instances WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("no instance named %s: %v", name, err)
	}
	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil || inst == nil {
		t.Fatalf("read instance %s: %v", name, err)
	}
	return inst
}

// jsonEqual compares two values by their JSON encoding, which is the shape the API promises
// and the one a nil-versus-empty difference should not fail on.
func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}
