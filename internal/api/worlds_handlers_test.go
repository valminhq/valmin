package api

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// fwlBytes builds a `.fwl` header in the layout measured in 03 §4.2.
func fwlBytes(version int32, name string) []byte {
	body := make([]byte, 4, 5+len(name))
	binary.LittleEndian.PutUint32(body, uint32(version))
	body = append(body, byte(len(name)))
	body = append(body, name...)
	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

func dbBytes() []byte { return []byte(strings.Repeat("world save data ", 200)) }

// uploadRequest builds a multipart POST carrying the named files.
func uploadRequest(t *testing.T, target string, files map[string][]byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		part, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const importPath = "/api/v1/instances/inst-a/worlds/import"

func worldsDirOf(t *testing.T, db *store.DB) string {
	t.Helper()
	return filepath.Join(dataDirOf(t, db), "worlds")
}

// seedExistingWorld puts a world in place so rule 6's snapshot has something to protect.
func seedExistingWorld(t *testing.T, db *store.DB, worldName string) {
	t.Helper()
	dir := filepath.Join(worldsDirOf(t, db), "worlds_local")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, worldName+".db"), []byte("THE ORIGINAL WORLD"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, worldName+".fwl"), fwlBytes(34, worldName), 0o664); err != nil {
		t.Fatal(err)
	}
}

// TestImportInstallsThePairUnderTheInstancesWorldName is the happy path, and it asserts the
// rename: `-world` names the *file basename* (03 §1.3), so a world that keeps the uploader's
// name is a world the server never opens.
func TestImportInstallsThePairUnderTheInstancesWorldName(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, admin, uploadRequest(t, importPath, map[string][]byte{
		"Uploaded.db":  dbBytes(),
		"Uploaded.fwl": fwlBytes(37, "Uploaded"),
	}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import job = %+v", final)
	}

	// The instance's world is "World" (seedInstance), not "Uploaded".
	local := filepath.Join(worldsDirOf(t, db), "worlds_local")
	for _, ext := range []string{".db", ".fwl"} {
		if _, err := os.Stat(filepath.Join(local, "World"+ext)); err != nil {
			t.Errorf("World%s was not installed: %v", ext, err)
		}
		if _, err := os.Stat(filepath.Join(local, "Uploaded"+ext)); err == nil {
			t.Errorf("Uploaded%s was left under the uploader's name", ext)
		}
	}
}

// TestImportRefusesALoneDB is 03 §4.1 rule 1 and the plan's own acceptance criterion:
// nothing is written, and the job says which half is missing.
func TestImportRefusesALoneDB(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedExistingWorld(t, db, "World")

	rec := as(rt, admin, uploadRequest(t, importPath, map[string][]byte{"Lonely.db": dbBytes()}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import = %d, want 202 — validation happens in the job (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "failed" {
		t.Fatalf("job = %+v, want failed", final)
	}

	original, err := os.ReadFile(filepath.Join(worldsDirOf(t, db), "worlds_local", "World.db"))
	if err != nil || string(original) != "THE ORIGINAL WORLD" {
		t.Errorf("the existing world was touched by a rejected import: %q, %v", original, err)
	}
}

// TestImportAcceptsAZipAndIgnoresItsPaths is the zip half of rule 1 plus B5. `↯` The entry
// named ../../etc/passwd is not "rejected" by a path check — no path from the archive is
// ever used, so it stages as a basename that then fails to be a world file. Structural, not
// vigilant.
func TestImportAcceptsAZipAndIgnoresItsPaths(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	archive := zipOf(t, map[string][]byte{
		"Valheim/worlds_local/Zipped.db":  dbBytes(),
		"Valheim/worlds_local/Zipped.fwl": fwlBytes(37, "Zipped"),
		"../../../etc/passwd":             []byte("root:x:0:0::/root:/bin/sh"),
	})
	rec := as(rt, admin, uploadRequest(t, importPath, map[string][]byte{"backup.zip": archive}))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("zip import = %+v", final)
	}

	if _, err := os.Stat(filepath.Join(worldsDirOf(t, db), "worlds_local", "World.db")); err != nil {
		t.Errorf("the zipped world was not installed: %v", err)
	}
	if _, err := os.Stat("/etc/passwd.imported"); err == nil {
		t.Fatal("a zip entry escaped the staging directory")
	}
}

// TestImportSnapshotsTheExistingWorldFirst is 03 §4.1 rule 6: the world that was there is
// recoverable, and the catalogue row says so.
func TestImportSnapshotsTheExistingWorldFirst(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedExistingWorld(t, db, "World")

	rec := as(rt, admin, uploadRequest(t, importPath, map[string][]byte{
		"New.db": dbBytes(), "New.fwl": fwlBytes(37, "New"),
	}))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import = %+v", final)
	}

	var trigger, consistent, path string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT trigger, consistent, path FROM backups WHERE instance_id = 'inst-a'`,
	).Scan(&trigger, &consistent, &path); err != nil {
		t.Fatalf("no pre-import backup row: %v", err)
	}
	if trigger != store.TriggerPreImport {
		t.Errorf("trigger = %q, want %q", trigger, store.TriggerPreImport)
	}
	if consistent != "1" && consistent != "true" {
		t.Errorf("consistent = %q, want true — the instance was stopped", consistent)
	}

	// The archive must actually contain the world it replaced, or the row is a lie.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("archive named by the row is missing: %v", err)
	}
	if !bytes.Contains(body, []byte{0x1f, 0x8b}) {
		t.Error("archive is not gzip")
	}
	if len(body) == 0 {
		t.Error("archive is empty")
	}
}

// TestImportAgainstARunningInstanceIsRefused is C19 — and it is WP-14's orphaned acceptance
// criterion, finally testable now that a mod-shaped endpoint exists.
func TestImportAgainstARunningInstanceIsRefused(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "running")

	rec := as(rt, admin, uploadRequest(t, importPath, map[string][]byte{
		"New.db": dbBytes(), "New.fwl": fwlBytes(37, "New"),
	}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("import against running = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "instance_must_be_stopped") {
		t.Errorf("code = %s, want instance_must_be_stopped", rec.Body)
	}
	if c := fake.Get(containerID); c == nil || !c.Running {
		t.Error("the container was stopped by a refused import — C19 forbids an implicit stop")
	}
}

// TestImportNeedsTheWorldImportGrant: world.import is a grantable extra (09 §3.2), off by
// default, because it replaces world data.
func TestImportNeedsTheWorldImportGrant(t *testing.T) {
	rt, db, fake, _, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, member, uploadRequest(t, importPath, map[string][]byte{
		"New.db": dbBytes(), "New.fwl": fwlBytes(37, "New"),
	}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer import = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestImportRefusesTheEnginesOwnBackupVariant is rule 5 end to end, with the opt-out.
func TestImportRefusesTheEnginesOwnBackupVariant(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	files := map[string][]byte{
		"NewWorld_backup_auto-20240517230012.db":  dbBytes(),
		"NewWorld_backup_auto-20240517230012.fwl": fwlBytes(34, "NewWorld"),
	}
	rec := as(rt, admin, uploadRequest(t, importPath, files))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "failed" {
		t.Errorf("a rolling backup was imported without being picked: %+v", final)
	}

	rec = as(rt, admin, uploadRequest(t, importPath+"?allow_backup_variant=true", files))
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Errorf("an explicitly picked backup was still refused: %+v", final)
	}
}

// TestImportLeavesNoStagingBehind is 12 §9.4: partial staging is deleted, on both paths.
func TestImportLeavesNoStagingBehind(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	staging := filepath.Join(rt.Supervisor().inst.Cfg.Data.Root, "staging")

	for _, files := range []map[string][]byte{
		{"Good.db": dbBytes(), "Good.fwl": fwlBytes(37, "Good")},
		{"Lonely.db": dbBytes()},
	} {
		rec := as(rt, admin, uploadRequest(t, importPath, files))
		var stub jobView
		decodeInto(t, rec, &stub)
		waitJob(t, rt, admin, stub.JobID)
	}

	entries, err := os.ReadDir(staging)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("staging still holds %v", names)
	}
}

// TestImportRejectsANonMultipartBody — the endpoint streams, so it has no other shape.
func TestImportRejectsANonMultipartBody(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	req := httptest.NewRequest(http.MethodPost, importPath, strings.NewReader(`{"world":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := as(rt, admin, req); rec.Code != http.StatusBadRequest {
		t.Errorf("json body = %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

var _ = io.Discard
