//go:build integration

// WP-M2-12: `05` M2's "Done when", as tests.
//
// AT-M2-1 and AT-M2-3 run against the **real downloaded packages** rather than the
// generated fixtures the rest of the suite uses. That is the whole point of them: the
// generated ones are shaped the way the placement rules expect, and every layout surprise
// M2 has had — F1's root `.dll` beside a top-level `config/`, F2's BOM, F3's backslash
// entries — came from a real archive nobody had looked inside. `↯` The corpus is never
// committed (ADR-105), so these skip unless VALMIN_MOD_CORPUS names a directory of zips;
// `make test-integration` in CI runs everything else.
//
// AT-M2-2 needs a real daemon and the stub image. AT-M2-4 needs a panel process that can be
// SIGKILLed and lives in cmd/valmind, beside M1's own crash tests.
package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// corpusPackage is one downloaded archive: its ident, the dependencies its own
// manifest.json declares, and the real bytes a fake CDN will serve for it.
type corpusPackage struct {
	fullName string
	version  string
	deps     []string
	body     []byte
	// alias marks a version row that stands in for a version of a package the corpus does
	// not hold at that number. See requestedVersions.
	alias bool
}

func (p corpusPackage) ident() string { return p.fullName + "-" + p.version }

// readCorpus loads every archive in VALMIN_MOD_CORPUS, or skips the test.
//
// `↯` Dependencies come from each archive's **own manifest.json**, not from a fixture list
// written here. In production they come from the synced Thunderstore index, which is the
// same data from the other end; taking them from the package means the closure these tests
// resolve is the one the packages actually declare, so a corpus refreshed next month
// changes the tree under test rather than silently continuing to assert last month's.
func readCorpus(t *testing.T) []corpusPackage {
	t.Helper()
	dir := os.Getenv("VALMIN_MOD_CORPUS")
	if dir == "" {
		t.Skip("VALMIN_MOD_CORPUS not set; skipping the real-package acceptance suite (ADR-105)")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no .zip files in %s", dir)
	}

	pkgs := make([]corpusPackage, 0, len(paths))
	for _, p := range paths {
		fullName, version, ok := splitCorpusName(filepath.Base(p))
		if !ok {
			t.Fatalf("%s is not named Namespace-Name-Version.zip", filepath.Base(p))
		}
		body, err := os.ReadFile(p) //nolint:gosec // a path this test globbed itself
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, corpusPackage{
			fullName: fullName, version: version, deps: corpusDependencies(t, p), body: body,
		})
	}
	return append(pkgs, requestedVersions(pkgs)...)
}

// requestedVersions fills in the versions the corpus *asks for* but does not hold.
//
// `↯` This is a property of the corpus, not of the panel. Thunderstore's index carries
// every version of every package; a directory of fifteen archives carries one each — and
// the corpus asks for four different BepInEx packs (5.4.1600, 2201, 2202, 2333) and two
// Jotunns. Without these rows the resolver is correct to refuse, and the acceptance suite
// would be asserting that a hole in the fixture data cannot be resolved.
//
// Each stand-in serves the bytes of the version that *is* on disk, under the requested
// number. That is a lie about the version and deliberately harmless here: what AT-M2-3
// asserts is what happens to a file tree, and the tree is the real package's either way.
// `↯` AT-M2-1's graph runs entirely on exact, real version matches and touches none of
// these.
func requestedVersions(pkgs []corpusPackage) []corpusPackage {
	have := map[string]corpusPackage{}
	for _, p := range pkgs {
		have[p.ident()] = p
	}
	byName := map[string]corpusPackage{}
	for _, p := range pkgs {
		byName[p.fullName] = p
	}

	var extra []corpusPackage
	for _, p := range pkgs {
		for _, dep := range p.deps {
			if _, ok := have[dep]; ok {
				continue
			}
			cut := strings.LastIndex(dep, "-")
			if cut < 1 {
				continue
			}
			source, ok := byName[dep[:cut]]
			if !ok {
				continue // the package itself is absent; the install will fail and say so
			}
			stand := corpusPackage{
				fullName: source.fullName, version: dep[cut+1:], deps: source.deps,
				body: source.body, alias: true,
			}
			have[stand.ident()] = stand
			extra = append(extra, stand)
		}
	}
	return extra
}

// splitCorpusName turns "Namespace-Name-1.2.3.zip" into its ident halves. The version is
// everything after the last hyphen — 03 §6.2's own convention, and the one Thunderstore's
// download URLs use.
func splitCorpusName(base string) (fullName, version string, ok bool) {
	trimmed := strings.TrimSuffix(base, ".zip")
	cut := strings.LastIndex(trimmed, "-")
	if cut < 1 || cut == len(trimmed)-1 {
		return "", "", false
	}
	return trimmed[:cut], trimmed[cut+1:], true
}

// corpusDependencies reads a package's declared dependencies out of its manifest.json.
// `↯` The BOM strip is F2, measured: Jotunn and XPortal both begin EF BB BF, and
// encoding/json fails outright on one.
func corpusDependencies(t *testing.T, zipPath string) []string {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open %s: %v", zipPath, err)
	}
	defer func() { _ = zr.Close() }()

	for _, f := range zr.File {
		if !strings.EqualFold(path.Base(f.Name), "manifest.json") || strings.Contains(f.Name, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open manifest in %s: %v", zipPath, err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read manifest in %s: %v", zipPath, err)
		}
		var manifest struct {
			Dependencies []string `json:"dependencies"`
		}
		if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &manifest); err != nil {
			t.Fatalf("decode manifest in %s: %v", zipPath, err)
		}
		return manifest.Dependencies
	}
	return nil
}

// corpusIndex serves the real archives and seeds the rows the resolver reads, so an install
// against this world downloads bytes that came off Thunderstore.
func corpusIndex(t *testing.T, db *store.DB, pkgs []corpusPackage) {
	t.Helper()
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
		bodies[p.ident()] = p.body
		deps, err := json.Marshal(p.deps)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, store.ModVersion{
			FullName: p.fullName, Version: p.version, DependenciesJSON: string(deps),
			DownloadURL: srv.URL + "/" + p.ident(), FileSize: int64(len(p.body)),
		})
		if p.alias {
			// A version row only. The package row must keep the version that is really on
			// disk as its latest — the BepInEx auto-install rule reads it, and a stand-in
			// winning that field would install a version nobody asked for.
			continue
		}
		namespace, name, _ := strings.Cut(p.fullName, "-")
		packages = append(packages, store.ModPackage{
			FullName: p.fullName, Namespace: namespace, Name: name,
			LatestVersion: p.version, CategoriesJSON: "[]",
		})
	}
	if err := db.UpsertModPackages(t.Context(), packages, versions); err != nil {
		t.Fatal(err)
	}
}

// corpusWorld is installWorld over the real archives: a stopped instance, the downloaded
// packages served by a fake CDN, and the index rows pointing at it.
func corpusWorld(t *testing.T, pkgs []corpusPackage) (
	rt *Router, db *store.DB, admin *store.User, dataDir string,
) {
	t.Helper()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	corpusIndex(t, db, pkgs)
	return rt, db, admin, filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot, "instances", "inst-a")
}

// serverFiles lists every file under an instance's server/ as slash-separated paths.
func serverFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	root := filepath.Join(dataDir, "server")
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// manifestOf decodes one installed package's recorded file manifest.
func manifestOf(t *testing.T, row store.InstanceMod) []installer.ManifestEntry {
	t.Helper()
	var entries []installer.ManifestEntry
	if err := json.Unmarshal([]byte(row.FileManifest), &entries); err != nil {
		t.Fatalf("%s has an undecodable manifest: %v", row.FullName, err)
	}
	return entries
}

// TestATM21ThreeDeepTreePlacesEveryFile is `05` M2's first "Done when", against the real
// three-deep tree F4 found in the corpus: OdinArchitect -> Jotunn -> the BepInEx pack.
//
// "Places every file correctly" is asserted in both directions — every path the manifests
// claim exists with the bytes they claim, **and** nothing else appeared under server/. One
// direction alone would pass a package that placed its files twice.
func TestATM21ThreeDeepTreePlacesEveryFile(t *testing.T) {
	pkgs := readCorpus(t)
	rt, db, admin, dataDir := corpusWorld(t, pkgs)

	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")

	rows := installedRows(t, db)
	if len(rows) != 3 {
		t.Fatalf("installed %d packages, want the three-deep closure: %v", len(rows), rows)
	}
	for fullName, want := range map[string]string{
		"OdinPlus-OdinArchitect":       store.InstalledExplicit,
		"ValheimModding-Jotunn":        store.InstalledDependency,
		"denikson-BepInExPack_Valheim": store.InstalledDependency,
	} {
		row, ok := rows[fullName]
		if !ok {
			t.Fatalf("%s is not installed; the closure stopped short", fullName)
		}
		if row.InstalledAs != want {
			t.Errorf("%s installed_as = %q, want %q", fullName, row.InstalledAs, want)
		}
	}

	// The destinations ADR-106's three heuristics produce for these three real layouts.
	// Named explicitly rather than derived from the manifest, so a placement change has to
	// be a deliberate edit here rather than a manifest that quietly agrees with itself.
	for _, want := range []string{
		"BepInEx/plugins/OdinArchitect/OdinArchitect.dll", // heuristic 3, a subdirectory
		"BepInEx/plugins/Jotunn.dll",                      // heuristic 3, a loose file
		"BepInEx/core/BepInEx.Preloader.dll",              // heuristic 1, the wrapper
		"doorstop_libs/libdoorstop_x64.so",
		"winhttp.dll",
	} {
		if _, err := os.Stat(serverPath(dataDir, want)); err != nil {
			t.Errorf("%s was not placed: %v", want, err)
		}
	}

	// Every manifested path exists, with the bytes the manifest recorded.
	claimed := map[string]bool{}
	for _, row := range rows {
		for _, e := range manifestOf(t, row) {
			claimed[e.Path] = true
			body, err := os.ReadFile(serverPath(dataDir, e.Path)) //nolint:gosec // the test's own tree
			if err != nil {
				t.Errorf("%s claims %s, which is not there: %v", row.FullName, e.Path, err)
				continue
			}
			if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != e.SHA256 {
				t.Errorf("%s: %s does not match its recorded hash", row.FullName, e.Path)
			}
		}
	}

	// And nothing landed that no manifest claims. `↯` This is the half that makes uninstall
	// exact (B9, ADR-009): a file on disk that no manifest names is a file uninstall will
	// leave behind forever.
	for _, got := range serverFiles(t, dataDir) {
		if !claimed[got] {
			t.Errorf("%s is under server/ and no package's manifest claims it", got)
		}
	}
}

// TestATM23UninstallReturnsEveryPackageByteIdentical is `05` M2's third "Done when", over
// every archive in the corpus rather than over the five that drove the heuristics.
//
// `↯` The tree is hashed before the install and after the uninstall, and `remove_orphans`
// takes the framework package with it — so the claim is that a vanilla server that installs
// a mod and changes its mind is left with the file tree it started with, byte for byte,
// including the merged `BepInEx/plugins/` case where two packages share a directory.
func TestATM23UninstallReturnsEveryPackageByteIdentical(t *testing.T) {
	pkgs := readCorpus(t)
	for _, pkg := range pkgs {
		if pkg.alias {
			continue // a stand-in version row, not an archive on disk
		}
		t.Run(pkg.ident(), func(t *testing.T) {
			rt, db, admin, dataDir := corpusWorld(t, pkgs)
			// A tree that is not empty to begin with: returning to empty is a weaker claim
			// than returning to what was there, and the operator's own config file is the
			// case B10 exists for.
			writeServerFile(t, dataDir, "valheim_server.x86_64", "the game")
			writeServerFile(t, dataDir, "BepInEx/config/Operator.cfg", "written by hand")
			before := serverTree(t, dataDir)

			installClosure(t, rt, admin, pkg.fullName, pkg.version)
			if after := serverTree(t, dataDir); after == before {
				t.Fatalf("%s installed nothing; the round trip would pass vacuously", pkg.ident())
			}

			rec := deleteMod(t, rt, admin, pkg.fullName, "?remove_orphans=true")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("uninstall = %d, want 202 (%s)", rec.Code, rec.Body)
			}
			var accepted jobView
			decodeInto(t, rec, &accepted)
			if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
				t.Fatalf("uninstall job = %+v, want succeeded", got)
			}

			if rows := installedRows(t, db); len(rows) != 0 {
				t.Errorf("%d rows survived an uninstall with remove_orphans: %v", len(rows), rows)
			}
			if after := serverTree(t, dataDir); after != before {
				t.Errorf("server/ is not byte-identical after the round trip.\nbefore:\n%s\nafter:\n%s",
					before, after)
			}
		})
	}
}

// moddedInstance is seedRealInstance with the bind mount that makes a mod visible to the
// server: the panel writes into <data_dir>/server and the container reads it at
// /opt/valheim/server (08 §5). Ports are deliberately not published — two acceptance runs
// on one host would collide on 2456, and nothing here joins a game.
func moddedInstance(t *testing.T, rt *Router, db *store.DB, d *runtime.Docker, name string) string {
	t.Helper()
	dataDir := filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot, "instances", name)
	// `↯` 0777 on the server tree, and only here. In production this directory is created by
	// a provision running as uid 10000 and is never chowned afterwards (Q14, A4), so the
	// container owns it outright. A test process is not 10000, so without this the stub
	// could not write the loader log the panel is about to read — and the test would be
	// asserting a permission failure rather than load verification.
	if err := os.MkdirAll(filepath.Join(dataDir, "server"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dataDir, "server"), 0o777); err != nil {
		t.Fatal(err)
	}

	containerID, err := d.Create(t.Context(), &runtime.ContainerSpec{
		Name:  instance.ContainerName(name) + "-" + nameSuffix(),
		Image: integrationGameImage, Labels: instance.Labels(name, 2456),
		Binds:      []runtime.Bind{{HostPath: dataDir + "/server", ContainerPath: "/opt/valheim/server"}},
		StopSignal: "SIGINT", StopTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), containerID, true) })

	seed(t, db, `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, 2456, 'Server', 'World', 'v1.k.n.ct', ?, ?, ?)`,
		name, name, containerID, dataDir, "cp-"+name, store.Now(), store.Now())
	return dataDir
}

// TestATM22AModdedServerBootsWithItsPluginsLoaded is `05` M2's second "Done when" as far as
// a stub can carry it: install a closure, start the real container, and read back which
// mods the server said it loaded.
//
// `↯` What makes this more than a fixture agreeing with itself: **nothing tells the stub
// which plugins to announce.** It lists the `.dll` files under BepInEx/plugins/ on the bind
// — the ones the installer placed — and writes its chainloader lines to BepInEx's own log
// file, which is where the panel reads them from (ADR-110). The panel then matches those
// names back to packages through the heuristic in `instance.PluginLoad`, so a placement
// change, a manifest change or a matching change all break this test.
//
// `↯` It does **not** prove Doorstop injection. Only the real image and the real BepInEx
// pack can, and that is docs/M2-VERIFICATION.md's manual leg.
func TestATM22AModdedServerBootsWithItsPluginsLoaded(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	name := "m2-load-" + nameSuffix()
	dataDir := moddedInstance(t, rt, db, d, name)

	fixtures := threeDeep()
	corpusIndexFromFixtures(t, db, fixtures)

	install := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/"+name+"/mods",
		jsonBody(t, installBody("OdinPlus-OdinArchitect", "1.7.0"))))
	if install.Code != http.StatusAccepted {
		t.Fatalf("install = %d, want 202 (%s)", install.Code, install.Body)
	}
	var accepted jobView
	decodeInto(t, install, &accepted)
	if got := waitForJobTerminal(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("install job = %+v, want succeeded", got)
	}
	// See moddedInstance: the installer created these as the test user, and the server runs
	// as 10000. Production has this the other way round and needs no such step.
	openTree(t, filepath.Join(dataDir, "server"))

	if final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+name+"/start"); final.Status != "succeeded" {
		t.Fatalf("start job = %+v, want succeeded", final)
	}
	t.Cleanup(func() { _ = runJobQuietly(rt, admin, http.MethodPost, "/api/v1/instances/"+name+"/stop") })

	mods, load := listModsOf(t, rt, admin, name)
	if load == nil {
		t.Fatal("the panel read no load report from a server that has booted with mods")
	}
	if load.Discrepancy != nil {
		t.Errorf("discrepancy = %q; the count line and the plugin lines disagree", *load.Discrepancy)
	}
	if load.Declared == nil || *load.Declared != load.Loaded {
		t.Errorf("declared = %v, loaded = %d; want them to agree", load.Declared, load.Loaded)
	}

	// The two packages that place a plugin loaded; the framework package places none and so
	// has no answer to give — null, which is not the same claim as not_seen (ADR-110).
	for fullName, want := range map[string]string{
		"OdinPlus-OdinArchitect":       LoadLoaded,
		"ValheimModding-Jotunn":        LoadLoaded,
		"denikson-BepInExPack_Valheim": "null",
	} {
		if got := statusOf(t, mods, fullName); got != want {
			t.Errorf("%s load_status = %q, want %q", fullName, got, want)
		}
	}
}

// corpusIndexFromFixtures is corpusIndex over generated packages, for the one acceptance
// test whose subject is the boot rather than the archives.
func corpusIndexFromFixtures(t *testing.T, db *store.DB, pkgs []modPackageFixture) {
	t.Helper()
	real := make([]corpusPackage, 0, len(pkgs))
	for _, p := range pkgs {
		real = append(real, corpusPackage{
			fullName: p.fullName, version: p.version, deps: p.deps, body: modZip(t, p.files),
		})
	}
	corpusIndex(t, db, real)
}

// openTree makes an installed tree writable by the container's uid. See moddedInstance.
func openTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(p, 0o777)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// listModsOf is listMods for an instance other than the fixture's inst-a.
func listModsOf(t *testing.T, rt *Router, u *store.User, instanceID string) (
	mods map[string]installedModView, load *pluginLoadView,
) {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/"+instanceID+"/mods", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Mods       []installedModView `json:"mods"`
		PluginLoad *pluginLoadView    `json:"plugin_load"`
	}
	decodeInto(t, rec, &body)
	mods = map[string]installedModView{}
	for _, m := range body.Mods {
		mods[m.FullName] = m
	}
	return mods, body.PluginLoad
}

// runJobQuietly is runJob for a cleanup step, where a failure is not the test's subject.
func runJobQuietly(rt *Router, u *store.User, method, path string) error {
	rec := as(rt, u, httptest.NewRequest(method, path, http.NoBody))
	if rec.Code != http.StatusAccepted {
		return io.EOF
	}
	return nil
}
