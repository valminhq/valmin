package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// modZip builds a Thunderstore-shaped package in memory. Generated rather than committed:
// ADR-105 keeps real archives out of the repository, and these are a few hundred bytes.
func modZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// modPackageFixture is one package the fake Thunderstore CDN serves and one row of the
// cached index.
type modPackageFixture struct {
	fullName string
	version  string
	deps     []string
	files    map[string]string
}

// installWorld is lifecycleWorld with a stopped instance, a fake CDN serving each fixture's
// zip, and mod_versions rows pointing at it — everything a mod_install run needs except a
// real Docker daemon or a real Thunderstore.
func installWorld(t *testing.T, pkgs ...modPackageFixture) (
	rt *Router, db *store.DB, admin, member *store.User, dataDir string,
) {
	t.Helper()
	var fake *runtime.Fake
	rt, db, fake, admin, member = lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	dataDir = filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot, "instances", "inst-a")

	bodies := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	packages := make([]store.ModPackage, 0, len(pkgs))
	versions := make([]store.ModVersion, 0, len(pkgs))
	for _, p := range pkgs {
		ident := p.fullName + "-" + p.version
		body := modZip(t, p.files)
		bodies[ident] = body
		deps, err := json.Marshal(p.deps)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, store.ModVersion{
			FullName: p.fullName, Version: p.version, DependenciesJSON: string(deps),
			DownloadURL: srv.URL + "/" + ident, FileSize: int64(len(body)),
		})
		// The index row too, not just the version: the BepInEx auto-install rule reads
		// latest_version off mod_packages, so a fixture with versions alone would make that
		// path fail for a reason no test meant to assert. Later fixtures for one package
		// overwrite earlier ones, so listing versions in ascending order leaves
		// latest_version on the newest.
		packages = append(packages, store.ModPackage{
			FullName: p.fullName, Namespace: "ns", Name: p.fullName,
			LatestVersion: p.version, CategoriesJSON: "[]",
		})
	}
	if err := db.UpsertModPackages(t.Context(), packages, versions); err != nil {
		t.Fatal(err)
	}
	return rt, db, admin, member, dataDir
}

// threeDeep is F4's real shape, generated: OdinArchitect -> Jotunn -> the BepInEx pack.
func threeDeep() []modPackageFixture {
	return []modPackageFixture{
		{
			fullName: "OdinPlus-OdinArchitect", version: "1.7.0",
			deps:  []string{"ValheimModding-Jotunn-2.29.2"},
			files: map[string]string{"manifest.json": `{"name":"OdinArchitect"}`, "plugins/OdinArchitect.dll": "odin"},
		},
		{
			fullName: "ValheimModding-Jotunn", version: "2.29.2",
			deps:  []string{"denikson-BepInExPack_Valheim-5.4.2333"},
			files: map[string]string{"manifest.json": `{"name":"Jotunn"}`, "plugins/Jotunn.dll": "jotunn"},
		},
		{
			fullName: "denikson-BepInExPack_Valheim", version: "5.4.2333",
			files: map[string]string{
				"manifest.json": `{"name":"BepInExPack_Valheim"}`,
				"BepInExPack_Valheim/BepInEx/core/BepInEx.Preloader.dll": "preloader",
				"BepInExPack_Valheim/doorstop_libs/libdoorstop_x64.so":   "doorstop",
				"BepInExPack_Valheim/winhttp.dll":                        "winhttp",
			},
		},
	}
}

// alreadyModded marks the instance as running BepInEx. A test about install mechanics
// should not also be exercising WP-M2-08's auto-install rule — a vanilla instance genuinely
// cannot take a mod without the framework, and that rule has its own tests.
func alreadyModded(t *testing.T, db *store.DB) {
	t.Helper()
	seed(t, db, `UPDATE instances SET modded = TRUE, bepinex_version = '5.4.2333' WHERE id = 'inst-a'`)
}

func installBody(fullName, version string) map[string]string {
	return map[string]string{"full_name": fullName, "version": version}
}

func postInstall(t *testing.T, rt *Router, u *store.User, fullName, version string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods",
		jsonBody(t, installBody(fullName, version))))
}

// serverTree fingerprints an instance's server/ directory the way the byte-identical
// criteria are stated.
func serverTree(t *testing.T, dataDir string) string {
	t.Helper()
	root := filepath.Join(dataDir, "server")
	var lines []string
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return ""
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		lines = append(lines, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func writeServerFile(t *testing.T, dataDir, rel, body string) {
	t.Helper()
	p := filepath.Join(dataDir, "server", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installedRows(t *testing.T, db *store.DB) map[string]store.InstanceMod {
	t.Helper()
	rows, err := db.InstanceMods(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.InstanceMod{}
	for _, r := range rows {
		out[r.FullName] = r
	}
	return out
}

// TestInstallPlacesTheWholeClosure is `05` M2's first "Done when", in the shape WP-M2-12
// will assert against the real binary: a three-deep tree installs every package, each with
// its own row and manifest, and the two that were pulled in are marked dependency.
func TestInstallPlacesTheWholeClosure(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)

	rec := postInstall(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("job = %+v, want succeeded", got)
	}

	for _, want := range []string{
		"BepInEx/plugins/OdinArchitect.dll",
		"BepInEx/plugins/Jotunn.dll",
		"BepInEx/core/BepInEx.Preloader.dll",
		"doorstop_libs/libdoorstop_x64.so",
		"winhttp.dll",
	} {
		if _, err := os.Stat(filepath.Join(dataDir, "server", filepath.FromSlash(want))); err != nil {
			t.Errorf("%s was not placed: %v", want, err)
		}
	}

	rows := installedRows(t, db)
	if len(rows) != 3 {
		t.Fatalf("instance_mods = %+v, want 3 rows", rows)
	}
	if got := rows["OdinPlus-OdinArchitect"].InstalledAs; got != store.InstalledExplicit {
		t.Errorf("OdinArchitect installed_as = %q, want explicit", got)
	}
	for _, dep := range []string{"ValheimModding-Jotunn", "denikson-BepInExPack_Valheim"} {
		if got := rows[dep].InstalledAs; got != store.InstalledDependency {
			t.Errorf("%s installed_as = %q, want dependency", dep, got)
		}
		if rows[dep].Side != store.SideUnknown {
			t.Errorf("%s side = %q; 03 §5.6 forbids the panel inferring it", dep, rows[dep].Side)
		}
	}

	// Every manifest hash must match what is actually on disk, or uninstall and rollback
	// are working from a record of something else.
	for name, row := range rows {
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(row.FileManifest), &manifest); err != nil {
			t.Fatalf("%s: manifest unreadable: %v", name, err)
		}
		if len(manifest) == 0 {
			t.Errorf("%s: empty manifest", name)
		}
		for _, e := range manifest {
			body, err := os.ReadFile(filepath.Join(dataDir, "server", filepath.FromSlash(e.Path)))
			if err != nil {
				t.Fatalf("%s: %s in the manifest is not on disk: %v", name, e.Path, err)
			}
			sum := sha256.Sum256(body)
			if got := hex.EncodeToString(sum[:]); got != e.SHA256 {
				t.Errorf("%s: %s hashes %s, manifest says %s", name, e.Path, got, e.SHA256)
			}
		}
	}

	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.RestartRequired {
		t.Error("restart_required is not set; ADR-012 wants the flag that says the change is not live yet")
	}
}

// TestInstallAgainstARunningInstanceIsRefused is B11 and C19: mods are applied to a stopped
// server, and a job never implicitly stops one.
func TestInstallAgainstARunningInstanceIsRefused(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	before := serverTree(t, dataDir)

	rec := postInstall(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "instance_must_be_stopped" {
		t.Errorf("code = %q, want instance_must_be_stopped", got)
	}
	if got := serverTree(t, dataDir); got != before {
		t.Error("the refused install still touched server/")
	}
	if rows := installedRows(t, db); len(rows) != 0 {
		t.Errorf("instance_mods = %+v, want none", rows)
	}
}

func TestInstallRequiresModsManage(t *testing.T) {
	rt, db, admin, member, _ := installWorld(t, threeDeep()...)

	if rec := postInstall(t, rt, member, "OdinPlus-OdinArchitect", "1.7.0"); rec.Code != http.StatusForbidden {
		t.Errorf("member without mods.manage: status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	grantModsManage(t, db)
	if rec := postInstall(t, rt, member, "OdinPlus-OdinArchitect", "1.7.0"); rec.Code != http.StatusAccepted {
		t.Errorf("member with mods.manage: status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	_ = admin
}

// TestInstallOnAnInvisibleInstanceIsNotFound is D2 / ADR-038: inst-b is real, and the
// member holds no grant on it, so it answers 404 rather than a 403 that would confirm it
// exists.
func TestInstallOnAnInvisibleInstanceIsNotFound(t *testing.T) {
	rt, db, _, member, _ := installWorld(t)
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-b', 'inst-b', 'stopped', '/srv/x', 2461, 'S', 'W', 'v1.k.n.ct', 'cp-b', ?, ?)`,
		store.Now(), store.Now())
	rec := as(rt, member, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-b/mods",
		jsonBody(t, installBody("A-A", "1.0.0"))))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// failMidApply is a closure whose second package cannot be placed, and only at the moment
// files actually move. Ns-First creates the directory BepInEx/plugins/Shared/; Ns-Second
// then has to publish a *file* at that same path, and a rename onto a directory fails.
// Nothing is detectable at plan time — the path does not exist when Diff looks — so the job
// gets all the way past the manifest and into the move, which is the only place the
// rollback path is reachable from.
func failMidApply() []modPackageFixture {
	return []modPackageFixture{
		{
			fullName: "Ns-First", version: "1.0.0", deps: []string{"Ns-Second-1.0.0"},
			files: map[string]string{
				"manifest.json":             "{}",
				"plugins/Shared/inside.dll": "inside the directory Ns-First creates",
				"plugins/Old.dll":           "the replacement Ns-First brings",
			},
		},
		{
			fullName: "Ns-Second", version: "1.0.0",
			files: map[string]string{"manifest.json": "{}", "plugins/Shared": "a file, where a directory now is"},
		},
	}
}

// TestInstallRollsBackByteIdentical is the criterion the manifest-before-move ordering
// exists for. The apply fails part-way through, after the first package has already been
// written and has already replaced a file that was there before — so returning server/
// byte-identical means restoring bytes, not just deleting paths.
func TestInstallRollsBackByteIdentical(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, failMidApply()...)
	alreadyModded(t, db)

	writeServerFile(t, dataDir, "BepInEx/plugins/Old.dll", "the version the operator already had")
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	before := serverTree(t, dataDir)

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "Ns-First", "1.0.0"), &accepted)
	got := waitJob(t, rt, admin, accepted.JobID)
	if got.Status != "failed" {
		t.Fatalf("job = %+v, want failed", got)
	}
	// The failure must be the move, not the plan — a job that died at 60 would satisfy every
	// assertion below without the rollback path ever running.
	if got.Progress < 85 {
		t.Fatalf("job failed at progress %d; it never reached the move phase (%+v)", got.Progress, got)
	}

	if tree := serverTree(t, dataDir); tree != before {
		t.Errorf("server/ after a rolled-back install:\n%s\nwant:\n%s", tree, before)
	}
	if rows := installedRows(t, db); len(rows) != 0 {
		t.Errorf("instance_mods = %+v, want none after a rollback", rows)
	}
}

// TestARolledBackInstallKeepsFilesItNeverDisplaced is the sharp edge of the ordering.
// Rollback reads "no backup for this manifest path" as "this path is ours, delete it".
//
// The closure applies First, Second, Third in that order. Second fails — it has to publish
// a file where First just made a directory — so Third's apply never runs, while Third's
// manifest is already recorded and names a path the operator already owns. If backups were
// taken inside each package's own apply, Third would have none, and the rollback would
// delete the operator's file. The backup pass covers the whole closure before the first
// manifest row precisely so it cannot.
func TestARolledBackInstallKeepsFilesItNeverDisplaced(t *testing.T) {
	pkgs := []modPackageFixture{
		{
			fullName: "Ns-First", version: "1.0.0",
			deps: []string{"Ns-Second-1.0.0", "Ns-Third-1.0.0"},
			files: map[string]string{
				"manifest.json":             "{}",
				"plugins/Shared/inside.dll": "inside the directory Ns-First creates",
			},
		},
		{
			fullName: "Ns-Second", version: "1.0.0",
			files: map[string]string{"manifest.json": "{}", "plugins/Shared": "a file, where a directory now is"},
		},
		{
			fullName: "Ns-Third",
			version:  "1.0.0",
			files: map[string]string{
				"manifest.json":        "{}",
				"plugins/Operator.dll": "what Ns-Third would have installed",
			},
		},
	}
	rt, db, admin, _, dataDir := installWorld(t, pkgs...)
	alreadyModded(t, db)

	const operators = "the operator's own build, which Ns-Third never got to replace"
	writeServerFile(t, dataDir, "BepInEx/plugins/Operator.dll", operators)
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	before := serverTree(t, dataDir)

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "Ns-First", "1.0.0"), &accepted)
	got := waitJob(t, rt, admin, accepted.JobID)
	if got.Status != "failed" {
		t.Fatalf("job = %+v, want failed", got)
	}
	if got.Progress < 85 {
		t.Fatalf("job failed at progress %d; it never reached the move phase (%+v)", got.Progress, got)
	}

	body, err := os.ReadFile(filepath.Join(dataDir, "server", "BepInEx", "plugins", "Operator.dll"))
	if err != nil {
		t.Fatalf("the rollback deleted a file the install never displaced: %v", err)
	}
	if string(body) != operators {
		t.Errorf("Operator.dll = %q, want the operator's own bytes %q", body, operators)
	}
	if tree := serverTree(t, dataDir); tree != before {
		t.Errorf("server/ after the rollback:\n%s\nwant:\n%s", tree, before)
	}
	if rows := installedRows(t, db); len(rows) != 0 {
		t.Errorf("instance_mods = %+v, want none after a rollback", rows)
	}
}

// TestInstallRefusesAPackageNameThatIsNotAPathSegment is B5 at the job level: the closure's
// names come from the cached index and become staging directories before Plan ever sees
// them, so a name with `..` would unpack a third-party zip outside the staging root.
func TestInstallRefusesAPackageNameThatIsNotAPathSegment(t *testing.T) {
	pkgs := []modPackageFixture{{
		fullName: "../../../../etc/cron.d", version: "1.0.0",
		files: map[string]string{"manifest.json": "{}", "Evil.dll": "payload"},
	}}
	rt, db, admin, _, _ := installWorld(t, pkgs...)
	alreadyModded(t, db)

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "../../../../etc/cron.d", "1.0.0"), &accepted)
	got := waitJob(t, rt, admin, accepted.JobID)
	if got.Status != "failed" {
		t.Fatalf("job = %+v, want failed", got)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "package_invalid" {
		t.Errorf("error code = %v, want package_invalid", got.ErrorCode)
	}
	if rows := installedRows(t, db); len(rows) != 0 {
		t.Errorf("instance_mods = %+v, want none", rows)
	}
}

// TestInstallOfAnAlreadyInstalledVersionIsANoOp: the resolver reports a satisfied node, so
// the job succeeds having downloaded nothing and changed nothing.
func TestInstallOfAnAlreadyInstalledVersionIsANoOp(t *testing.T) {
	pkgs := []modPackageFixture{{
		fullName: "Ns-Only", version: "1.0.0",
		files: map[string]string{"manifest.json": "{}", "plugins/Only.dll": "only"},
	}}
	rt, db, admin, _, dataDir := installWorld(t, pkgs...)
	alreadyModded(t, db)

	first := postInstall(t, rt, admin, "Ns-Only", "1.0.0")
	var accepted jobView
	decodeInto(t, first, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("first install = %+v, want succeeded", got)
	}
	after := serverTree(t, dataDir)

	second := postInstall(t, rt, admin, "Ns-Only", "1.0.0")
	decodeInto(t, second, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("second install = %+v, want succeeded", got)
	}
	if got := serverTree(t, dataDir); got != after {
		t.Error("re-installing the same version changed server/")
	}
	if rows := installedRows(t, db); len(rows) != 1 {
		t.Errorf("instance_mods = %+v, want one row", rows)
	}
}

// TestInstallOverADifferentVersionIsRefused. Changing an installed package's version is an
// update — uninstall-then-install, WP-M2-09's job — because the old version's files are
// only removable from the old version's manifest. Installing over it would orphan every
// file the new version does not also ship, and BepInEx would load them.
func TestInstallOverADifferentVersionIsRefused(t *testing.T) {
	pkgs := []modPackageFixture{
		{
			fullName: "Ns-Only", version: "1.0.0",
			files: map[string]string{"manifest.json": "{}", "plugins/Only.dll": "v1"},
		},
		{
			fullName: "Ns-Only", version: "2.0.0",
			files: map[string]string{"manifest.json": "{}", "plugins/Only.dll": "v2"},
		},
	}
	rt, db, admin, _, dataDir := installWorld(t, pkgs...)
	alreadyModded(t, db)

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "Ns-Only", "1.0.0"), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("first install = %+v, want succeeded", got)
	}
	after := serverTree(t, dataDir)

	decodeInto(t, postInstall(t, rt, admin, "Ns-Only", "2.0.0"), &accepted)
	got := waitJob(t, rt, admin, accepted.JobID)
	if got.Status != "failed" {
		t.Fatalf("upgrade = %+v, want failed", got)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "mod_conflict" {
		t.Errorf("error code = %v, want mod_conflict", got.ErrorCode)
	}
	if tree := serverTree(t, dataDir); tree != after {
		t.Error("the refused upgrade changed server/")
	}
	if rows := installedRows(t, db); rows["Ns-Only"].Version != "1.0.0" {
		t.Errorf("installed version = %q, want the untouched 1.0.0", rows["Ns-Only"].Version)
	}
}

// TestModInstallCancelPolicy is 12 §8's row for this kind, as a table. The boundary is the
// manifest: once it is recorded, files are moving and the rollback path owns the outcome.
func TestModInstallCancelPolicy(t *testing.T) {
	for _, tt := range []struct {
		checkpoint string
		want       bool
	}{
		{"", true},
		{checkpointResolved, true},
		{checkpointDownloaded, true},
		{checkpointStaged, true},
		{checkpointManifestWritten, false},
		{checkpointApplied, false},
	} {
		t.Run(tt.checkpoint, func(t *testing.T) {
			got, phase := modInstallCancelPolicy(tt.checkpoint)
			if got != tt.want {
				t.Errorf("cancellable at %q = %v, want %v", tt.checkpoint, got, tt.want)
			}
			if !got && phase == "" {
				t.Error("a refusal must name the phase (12 §8)")
			}
		})
	}
}

// TestListInstalledMods is GET /instances/{id}/mods: a viewer capability, so anyone who can
// see the instance can see what is on it.
func TestListInstalledMods(t *testing.T) {
	rt, db, admin, member, _ := installWorld(t, threeDeep()...)

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0"), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("install = %+v, want succeeded", got)
	}
	_ = db

	rec := as(rt, member, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Mods []installedModView `json:"mods"`
	}
	decodeInto(t, rec, &body)
	if len(body.Mods) != 3 {
		t.Fatalf("mods = %+v, want 3", body.Mods)
	}
	if body.Mods[0].FullName > body.Mods[1].FullName {
		t.Error("rows are not ordered by full name")
	}
	for _, m := range body.Mods {
		if m.FileCount == 0 {
			t.Errorf("%s: file_count = 0", m.FullName)
		}
		if !m.Enabled {
			t.Errorf("%s: enabled = false on a fresh install", m.FullName)
		}
	}
}

func TestInstallMissingFieldsIsValidationFailed(t *testing.T) {
	rt, _, admin, _, _ := installWorld(t)
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods",
		jsonBody(t, map[string]string{})))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// TestInstallLeavesNoStagingBehind: the staging area holds a full copy of every package
// plus backups of what it displaced, and a job that does not clean up after itself fills
// the disk one install at a time.
func TestInstallLeavesNoStagingBehind(t *testing.T) {
	rt, _, admin, _, _ := installWorld(t, threeDeep()...)

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0"), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("install = %+v, want succeeded", got)
	}

	root := modStagingRoot(rt.Supervisor().inst.Cfg.Data.Root)
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging root still holds %v", entries)
	}
}

// TestASweptModInstallIsRolledBack is 12 §9.4's "not resumed — roll back from the
// manifest", for the case the job could not handle itself because it was not running when
// the panel died. It plants exactly what a SIGKILL between the manifest and the end of the
// move leaves behind: rows written, backups taken, some files already replaced.
func TestASweptModInstallIsRolledBack(t *testing.T) {
	rt, db, _, _, dataDir := installWorld(t)

	writeServerFile(t, dataDir, "BepInEx/plugins/Old.dll", "the version already installed")
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	before := serverTree(t, dataDir)

	// What the interrupted job had already done: staged the package, backed up what it was
	// about to displace, written the row, and replaced one of the two files.
	root := modStagingRoot(rt.Supervisor().inst.Cfg.Data.Root)
	staging, err := os.MkdirTemp(mkdirAllT(t, root), "install-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "pkg", "Ns-Half"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(staging, "backup", "BepInEx", "plugins")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(backup, "Old.dll"),
		[]byte("the version already installed"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	writeServerFile(t, dataDir, "BepInEx/plugins/Old.dll", "the half-written replacement")
	writeServerFile(t, dataDir, "BepInEx/plugins/New.dll", "a file only the interrupted job wrote")

	manifest, err := json.Marshal([]installer.ManifestEntry{
		{Path: "BepInEx/plugins/Old.dll"}, {Path: "BepInEx/plugins/New.dll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteInstanceMods(t.Context(), "inst-a", []store.InstanceMod{{
		InstanceID: "inst-a", FullName: "Ns-Half", Version: "1.0.0",
		InstalledAs: store.InstalledExplicit, Side: store.SideUnknown, Enabled: true,
		FileManifest: string(manifest),
	}}); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(modInstallPayload{
		StagingDir: staging, FullName: "Ns-Half", Version: "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_install", checkpointManifestWritten, string(payload))

	if _, err := rt.Supervisor().sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after the sweep:\n%s\nwant:\n%s", got, before)
	}
	if rows := installedRows(t, db); len(rows) != 0 {
		t.Errorf("instance_mods = %+v, want none after a rolled-back install", rows)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("the staging directory survived the sweep: %v", err)
	}
}

// TestTheSweepRefusesAModStagingPathOutsideTheStagingRoot is B5 on the recovery path:
// staging_dir is a value read back out of the database and handed to a recursive delete.
func TestTheSweepRefusesAModStagingPathOutsideTheStagingRoot(t *testing.T) {
	rt, db, _, _, dataDir := installWorld(t)
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	elsewhere := t.TempDir()

	payload, err := json.Marshal(modInstallPayload{StagingDir: elsewhere, FullName: "Ns-X", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_install", checkpointManifestWritten, string(payload))

	if _, err := rt.Supervisor().sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Errorf("the sweep removed a directory outside the staging root: %v", err)
	}
}

func mkdirAllT(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
