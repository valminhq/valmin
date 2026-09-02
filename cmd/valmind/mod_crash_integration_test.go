//go:build integration

// WP-M2-12's AT-M2-4: the panel is SIGKILLed while a mod install is moving files into
// server/, and the boot that follows has to put the tree back.
//
// `↯` This is the test ADR-009 exists for, and it is here rather than in internal/api for
// the same reason M1's crash tests are: the claim is about a *process* dying, and an
// in-process test cannot be SIGKILLed. The rollback it proves is driven entirely by the
// file manifest the job wrote **before** it moved anything (12 §9.4) — remove that ordering
// and there is nothing on disk for the sweep to work from.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// modFixture is one package the fake CDN serves and one row of the index the resolver
// reads. The real Thunderstore is never reached from a test (ADR-105, `06 §4`).
type modFixture struct {
	fullName string
	version  string
	deps     []string
	files    map[string]string
}

func (f modFixture) ident() string { return f.fullName + "-" + f.version }

func zipOf(t *testing.T, files map[string]string) []byte {
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

// wideFixture is a package of many small files. `↯` The count is the whole point: the
// window this test has to land its SIGKILL inside is the time the job spends moving staged
// files into server/, and a three-file package closes that window in under a millisecond.
// Raise it if the assertions below start reporting a completed install.
const wideFixtureFiles = 6000

func wideFixture() modFixture {
	files := map[string]string{"manifest.json": `{"name":"Wide"}`}
	for i := range wideFixtureFiles {
		files[fmt.Sprintf("plugins/Wide/part-%04d.dat", i)] = fmt.Sprintf("payload %d\n", i)
	}
	return modFixture{
		fullName: "Acceptance-Wide", version: "1.0.0", files: files,
		deps: []string{"denikson-BepInExPack_Valheim-5.4.2333"},
	}
}

func bepinexFixture() modFixture {
	return modFixture{
		fullName: "denikson-BepInExPack_Valheim", version: "5.4.2333",
		files: map[string]string{
			"manifest.json": `{"name":"BepInExPack_Valheim"}`,
			"BepInExPack_Valheim/BepInEx/core/BepInEx.Preloader.dll": "preloader",
			"BepInExPack_Valheim/doorstop_libs/libdoorstop_x64.so":   "doorstop",
			"BepInExPack_Valheim/winhttp.dll":                        "winhttp",
		},
	}
}

// seedModIndex serves the fixtures and writes the index rows the resolver reads. It opens
// the panel's database directly, as seedInstance does, so it must run before the panel's
// first boot.
func seedModIndex(t *testing.T, p *panel, fixtures ...modFixture) {
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

	packages := make([]store.ModPackage, 0, len(fixtures))
	versions := make([]store.ModVersion, 0, len(fixtures))
	for _, f := range fixtures {
		body := zipOf(t, f.files)
		bodies[f.ident()] = body
		deps, err := json.Marshal(f.deps)
		if err != nil {
			t.Fatal(err)
		}
		namespace, name, _ := strings.Cut(f.fullName, "-")
		versions = append(versions, store.ModVersion{
			FullName: f.fullName, Version: f.version, DependenciesJSON: string(deps),
			DownloadURL: srv.URL + "/" + f.ident(), FileSize: int64(len(body)),
		})
		packages = append(packages, store.ModPackage{
			FullName: f.fullName, Namespace: namespace, Name: name,
			LatestVersion: f.version, CategoriesJSON: "[]",
		})
	}

	db := openPanelDB(t, p)
	defer func() { _ = db.Close() }()
	if err := db.UpsertModPackages(t.Context(), packages, versions); err != nil {
		t.Fatalf("seed the mod index: %v", err)
	}
}

// openPanelDB opens the panel's own database. Reading it while the daemon is live is safe —
// SQLite in WAL mode (10 §4.3) — and it is the only way to see a checkpoint as it is
// written rather than after the job has finished.
func openPanelDB(t *testing.T, p *panel) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(p.root, "panel.db"))
	if err != nil {
		t.Fatalf("open the panel database: %v", err)
	}
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// awaitCheckpoint polls the job row until it records want, then returns. `↯` It polls the
// database rather than the API: the checkpoint is not on the job resource, and at 300
// requests a minute (11 §7) an HTTP poll tight enough to catch a phase that lasts
// milliseconds would spend its whole budget and start decoding 429s as jobs.
func awaitCheckpoint(t *testing.T, db *store.DB, jobID, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		j, err := db.JobByID(context.Background(), jobID)
		if err == nil && j != nil {
			if j.Checkpoint != nil && *j.Checkpoint == want {
				return
			}
			switch j.Status {
			case "succeeded", "failed", "cancelled":
				t.Fatalf("job %s reached %s without ever recording checkpoint %q — the "+
					"install finished before the kill landed; raise wideFixtureFiles",
					jobID, j.Status, want)
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s never recorded checkpoint %q", jobID, want)
}

// treeHash fingerprints an instance's server/ the way the byte-identical criterion is
// stated: every file's path and content hash, ordered.
func treeHash(t *testing.T, dataDir string) string {
	t.Helper()
	root := filepath.Join(dataDir, "server")
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p) //nolint:gosec // the test's own tree
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		lines = append(lines, filepath.ToSlash(rel)+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestATM24CrashMidApplyRollsBackFromTheManifest is `05` M2's install half stated as a
// failure: the panel dies with staged files already moving into server/, and the tree it
// left behind has to come back byte-identical without anybody asking.
//
// `↯` Three separate claims, and the third is the one ADR-009 was argued over: the tree is
// restored, the rows do not appear for a package that never finished installing, and the
// restored tree still contains the operator's own file — the rollback is driven by a
// manifest of what this job placed, not by deleting everything that looks like a mod.
func TestATM24CrashMidApplyRollsBackFromTheManifest(t *testing.T) {
	p := newPanel(t, nil)
	d := docker(t)
	id, _ := seedInstance(t, p, d, "m2-crash-apply")
	seedModIndex(t, p, wideFixture(), bepinexFixture())

	dataDir := filepath.Join(p.root, "instances", id)
	// A server directory that is not empty: "returns to byte-identical" is a claim about
	// what was there, and an empty tree makes "delete everything" pass as a rollback.
	if err := os.MkdirAll(filepath.Join(dataDir, "server", "BepInEx", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"valheim_server.x86_64":       "the game",
		"BepInEx/config/Operator.cfg": "written by hand",
	} {
		path := filepath.Join(dataDir, "server", filepath.FromSlash(rel))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := treeHash(t, dataDir)

	p.start()
	p.setup()

	resp := p.do(http.MethodPost, "/api/v1/instances/"+id+"/mods",
		map[string]string{"full_name": "Acceptance-Wide", "version": "1.0.0"})
	if resp.status != http.StatusAccepted {
		t.Fatalf("install = %d, want 202 (%s)", resp.status, resp.body)
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(resp.body), &accepted); err != nil {
		t.Fatal(err)
	}

	db := openPanelDB(t, p)
	// `↯` manifest_written is the last checkpoint before any file moves (12 §9.4). Killing
	// on it is what makes this "mid-apply" rather than "before the work started": from here
	// the job is writing into server/, and the manifest naming what it will write is
	// already on disk.
	awaitCheckpoint(t, db, accepted.JobID, "manifest_written")
	p.kill()
	_ = db.Close()

	p.restart()

	if job := p.awaitJob(accepted.JobID); job.Status != "failed" ||
		job.ErrorCode == nil || *job.ErrorCode != "interrupted" {
		t.Errorf("the swept install = %+v, want failed/interrupted", job)
	}
	if log := p.out.String(); !strings.Contains(log, "rolled back an interrupted mod install") {
		t.Errorf("the boot after the crash rolled nothing back:\n%s", log)
	}
	if after := treeHash(t, dataDir); after != before {
		t.Errorf("server/ was not restored.\nbefore:\n%s\n\nafter:\n%s", before, after)
	}

	resp = p.do(http.MethodGet, "/api/v1/instances/"+id+"/mods", nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list mods = %d (%s)", resp.status, resp.body)
	}
	var listed struct {
		Mods []struct {
			FullName string `json:"full_name"`
		} `json:"mods"`
	}
	if err := json.Unmarshal([]byte(resp.body), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Mods) != 0 {
		t.Errorf("%d mods are recorded as installed after an interrupted install: %+v",
			len(listed.Mods), listed.Mods)
	}
	// And the staging directory the job was working in is gone (12 §9.4).
	staging, err := os.ReadDir(filepath.Join(p.root, "staging", "mods"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Errorf("%d staging directories survived the sweep", len(staging))
	}
}
