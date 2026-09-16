package api

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	apierr "github.com/valminhq/valmin/internal/api/errors"
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

func worldsDirOfID(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	return filepath.Join(dataDirOfID(t, db, id), "worlds")
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

// TestImportAcceptsAZipAndIgnoresItsPaths is the zip half of rule 1 plus B5. The entry
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

// TestImportAgainstARunningInstanceIsRefused is C19 — and it is the orphaned acceptance
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

// Asserts an upload past the cap is refused rather than written as a prefix. io.Copy over an
// io.LimitReader stops at the limit and reports success, so the cap has to be tested by
// reading one byte past it (11 §8.3).
func TestAnOversizedUploadIsRefusedNotTruncated(t *testing.T) {
	const limit = 64
	dir := t.TempDir()

	over := filepath.Join(dir, "Over.db")
	err := writeStaged(bytes.NewReader(bytes.Repeat([]byte("x"), limit+1)), over, limit)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.PayloadTooLarge {
		t.Fatalf("writeStaged past the cap = %v, want payload_too_large", err)
	}

	// Exactly at the cap is the largest thing that is not over it, and must land whole.
	at := filepath.Join(dir, "At.db")
	if err := writeStaged(bytes.NewReader(bytes.Repeat([]byte("x"), limit)), at, limit); err != nil {
		t.Fatalf("writeStaged at the cap = %v, want it accepted", err)
	}
	info, err := os.Stat(at)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != limit {
		t.Errorf("a file at the cap landed as %d bytes, want %d", info.Size(), limit)
	}
}

// Asserts a read failure is not reported as an oversized upload: a disk or network error
// answered with 413 sends the operator looking for a file size that was never the problem.
func TestAFailedUploadReadIsNotReportedAsTooLarge(t *testing.T) {
	err := writeStaged(iotest.ErrReader(io.ErrUnexpectedEOF), filepath.Join(t.TempDir(), "x.db"), 1<<20)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.Internal {
		t.Errorf("writeStaged over a broken reader = %v, want internal", err)
	}
}

func TestImportUploadEnforcesAggregateByteLimit(t *testing.T) {
	const limit = 10
	request := uploadRequest(t, importPath, map[string][]byte{
		"World.db":  bytes.Repeat([]byte("d"), 6),
		"World.fwl": bytes.Repeat([]byte("f"), 6),
	})
	err := stageUploadWithLimits(request, t.TempDir(), limit, uploadEntryLimit)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.PayloadTooLarge {
		t.Fatalf("stageUploadWithLimits total above %d = %v, want payload_too_large", limit, err)
	}
}

func TestImportUploadEnforcesEntryLimit(t *testing.T) {
	request := uploadRequest(t, importPath, map[string][]byte{
		"One.db": []byte("1"), "One.fwl": []byte("2"), "Two.db": []byte("3"),
	})
	err := stageUploadWithLimits(request, t.TempDir(), 1<<20, 2)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.PayloadTooLarge {
		t.Fatalf("stageUploadWithLimits past entry cap = %v, want payload_too_large", err)
	}
}

// oneZeroUpload is a 1.0 world as it leaves a user's machine: a directory named after the
// world, holding the generation's two halves beside the chunk files that carry most of it
// (evidence/world-format-1.0-2026-09-14.md). The names are what a browser sends for a folder
// upload and what a zip of that folder carries.
func oneZeroUpload(world string) map[string][]byte {
	return map[string][]byte{
		world + "/_main.17.db2":      dbBytes(),
		world + "/_main.17.fwl2":     fwlBytes(41, world),
		world + "/_main.17.chunks":   []byte("chunk index"),
		world + "/_main.17.ok":       {0x29, 0, 0, 0},
		world + "/20_20__1_9.chunk":  []byte(strings.Repeat("chunk ", 200)),
		world + "/00_00__0_12.chunk": []byte(strings.Repeat("chunk ", 50)),
	}
}

// A 1.0 world is a directory and its name is the world's name, so an import that flattened
// uploads to basenames threw away the only record of which world it was. It lands under the
// name this instance loads, with its own file names kept: the save counter in `_main.<gen>.*`
// is the game's (ADR-180).
func TestImportInstallsAOneZeroWorld(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, admin, uploadRequest(t, importPath, oneZeroUpload("Worild1")))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import = %s / %s", final.Status, deref(final.Error))
	}

	// "World" is what the seeded instance is configured to load, so the directory is renamed
	// to it and the files inside are not.
	dir := filepath.Join(worldsDirOf(t, db), "worlds_local", "World")
	for _, name := range []string{
		"_main.17.db2", "_main.17.fwl2", "_main.17.chunks",
		"_main.17.ok", "20_20__1_9.chunk", "00_00__0_12.chunk",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not installed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(worldsDirOf(t, db), "worlds_local", "Worild1")); err == nil {
		t.Error("the world was installed under its uploaded name rather than the instance's")
	}
}

// The same world as a zip of the folder, which is what a user on Windows will reach for.
func TestImportInstallsAOneZeroWorldFromAZip(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	entries := map[string][]byte{"screenshot.png": []byte("not a world")}
	for name, content := range oneZeroUpload("Worild1") {
		entries["Valheim/worlds_local/"+name] = content
	}
	rec := as(rt, admin, uploadRequest(t, importPath, map[string][]byte{"save.zip": zipOf(t, entries)}))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("zip import = %+v", final)
	}

	dir := filepath.Join(worldsDirOf(t, db), "worlds_local", "World")
	if _, err := os.Stat(filepath.Join(dir, "_main.17.db2")); err != nil {
		t.Errorf("the zipped 1.0 world was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "screenshot.png")); err == nil {
		t.Error("a file that is not part of a world was installed")
	}
}

// A 1.0 world's file names carry a save counter, so writing one over another of the same name
// would leave two generations in one directory and the game would load whichever it preferred
// — a world that is neither. The import publishes by the same two renames a restore uses, so
// what is there afterwards is exactly the world that was uploaded (ADR-180).
func TestImportingAOneZeroWorldLeavesNoTraceOfTheOldOne(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	existing := filepath.Join(worldsDirOf(t, db), "worlds_local", "World")
	if err := os.MkdirAll(existing, 0o775); err != nil {
		t.Fatal(err)
	}
	// A later generation than the upload carries, which is the case that matters: left
	// behind, it is the one the game would read.
	for _, name := range []string{"_main.99.db2", "_main.99.fwl2", "ff_ff__9_9.chunk"} {
		if err := os.WriteFile(filepath.Join(existing, name), []byte("THE OLD WORLD"), 0o664); err != nil {
			t.Fatal(err)
		}
	}

	rec := as(rt, admin, uploadRequest(t, importPath, oneZeroUpload("Worild1")))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import = %+v", final)
	}

	for _, name := range []string{"_main.99.db2", "_main.99.fwl2", "ff_ff__9_9.chunk"} {
		if _, err := os.Stat(filepath.Join(existing, name)); err == nil {
			t.Errorf("%s from the replaced world survived the import", name)
		}
	}
	if _, err := os.Stat(filepath.Join(existing, "_main.17.db2")); err != nil {
		t.Errorf("the imported world is not there: %v", err)
	}
}

// TestRestoreAGameBackupFromDisk is the operator's own use of 03 §4.1 rule 5: the game keeps
// rolling saves beside the live world, and rolling back to one should not require downloading
// it and uploading it again.
func TestRestoreAGameBackupFromDisk(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	local := filepath.Join(worldsDirOf(t, db), "worlds_local")

	// The live world the instance loads, and one of the game's own backups of it holding
	// different bytes, in 1.0's directory layout.
	writeWorldDir(t, filepath.Join(local, "World"), 22, "current world")
	auto := "World_backup_auto-20260914-081726"
	writeWorldDir(t, filepath.Join(local, auto), 14, "older world")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost,
		"/api/v1/instances/inst-a/worlds/"+auto+"/restore", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("restore = %s / %s", final.Status, deref(final.Error))
	}

	// The backup's bytes are now the live world, under the name the instance loads.
	got, err := os.ReadFile(filepath.Join(local, "World", "_main.14.db2"))
	if err != nil {
		t.Fatalf("the backup was not installed as the live world: %v", err)
	}
	if !bytes.HasSuffix(got, []byte("older world")) {
		t.Error("_main.14.db2 does not carry the backup's bytes")
	}
	if _, err := os.Stat(filepath.Join(local, "World", "_main.22.db2")); err == nil {
		t.Error("the replaced world's files are still there: the install was not a swap")
	}
	// The backup itself is left alone, so the operator can roll back again.
	if _, err := os.Stat(filepath.Join(local, auto, "_main.14.db2")); err != nil {
		t.Errorf("restoring consumed the game's backup: %v", err)
	}
}

// A name that is not a world in this instance's savedir is a 404, and nothing is touched. The
// lookup is an exact key into the scan, so a traversal attempt cannot match a world.
func TestRestoreFromDiskRefusesAWorldThatIsNotThere(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	writeWorldDir(t, filepath.Join(worldsDirOf(t, db), "worlds_local", "World"), 22, "current world")

	for _, name := range []string{"Nope", "..%2f..%2fetc"} {
		rec := as(rt, admin, httptest.NewRequest(http.MethodPost,
			"/api/v1/instances/inst-a/worlds/"+name+"/restore", http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (%s)", name, rec.Code, rec.Body)
		}
	}
}

// TestDeleteTheLoadedWorldResetsTheServer: removing the world an instance loads is the
// supported way to start over, and the savedir is archived first so it is undoable.
func TestDeleteTheLoadedWorldResetsTheServer(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	local := filepath.Join(worldsDirOf(t, db), "worlds_local")
	writeWorldDir(t, filepath.Join(local, "World"), 22, "current world")

	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete,
		"/api/v1/instances/inst-a/worlds/World", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("delete = %s / %s", final.Status, deref(final.Error))
	}

	if _, err := os.Stat(filepath.Join(local, "World")); !os.IsNotExist(err) {
		t.Errorf("the world directory is still there: %v", err)
	}
	backups, err := db.ListBackups(t.Context(), "inst-a", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("catalogued %d backups, want the one taken before the delete", len(backups))
	}
}

// A pre-1.0 pair takes its `.old` fallbacks with it: those classify as no world at all, so
// leaving them behind leaves the deleted world's bytes on disk under names nothing lists.
func TestDeleteAPairTakesItsFallbacks(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	local := filepath.Join(worldsDirOf(t, db), "worlds_local")
	if err := os.MkdirAll(local, 0o775); err != nil {
		t.Fatal(err)
	}
	names := []string{"World.db", "World.fwl", "World.db.old", "World.fwl.old"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(local, name), dbBytes(), 0o664); err != nil {
			t.Fatal(err)
		}
	}
	// A second world, which the delete must leave alone.
	if err := os.WriteFile(filepath.Join(local, "Midgard.db"), dbBytes(), 0o664); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete,
		"/api/v1/instances/inst-a/worlds/World", http.NoBody))
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("delete = %s / %s", final.Status, deref(final.Error))
	}

	for _, name := range names {
		if _, err := os.Stat(filepath.Join(local, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the delete: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(local, "Midgard.db")); err != nil {
		t.Errorf("another world was removed too: %v", err)
	}
}

// A name that is not a world in this instance's savedir is a 404, and a running instance is
// refused before anything is touched (C19).
func TestDeleteWorldRefusesWhatItCannotName(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	local := filepath.Join(worldsDirOf(t, db), "worlds_local")
	writeWorldDir(t, filepath.Join(local, "World"), 22, "current world")

	for _, name := range []string{"Nope", "..%2f..%2fetc"} {
		rec := as(rt, admin, httptest.NewRequest(http.MethodDelete,
			"/api/v1/instances/inst-a/worlds/"+name, http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (%s)", name, rec.Code, rec.Body)
		}
	}

	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete,
		"/api/v1/instances/inst-a/worlds/World", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 on a running instance (%s)", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(local, "World")); err != nil {
		t.Errorf("the world was touched anyway: %v", err)
	}
}

// writeWorldDir lays out one world in 1.0's directory layout (03 §4, ADR-179).
func writeWorldDir(t *testing.T, dir string, gen int, data string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		fmt.Sprintf("_main.%d.db2", gen):    append(dbBytes(), data...),
		fmt.Sprintf("_main.%d.fwl2", gen):   fwlBytes(41, filepath.Base(dir)),
		fmt.Sprintf("_main.%d.chunks", gen): []byte("chunk index"),
		fmt.Sprintf("_main.%d.ok", gen):     {0x29, 0, 0, 0},
		"20_20__1_9.chunk":                  []byte(strings.Repeat("chunk ", 200)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o664); err != nil {
			t.Fatal(err)
		}
	}
}
