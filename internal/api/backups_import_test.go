package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

const importBackupPath = "/api/v1/instances/inst-a/worlds/import-backup"

// archiveWorld is one world to put in a test archive.
type archiveWorld struct {
	name string
	db   []byte
}

// writeBackupArchive archives worlds the way a backup job does, at dest, and returns dest.
func writeBackupArchive(t *testing.T, dest string, worlds ...archiveWorld) string {
	t.Helper()
	tree := t.TempDir()
	local := filepath.Join(tree, instance.WorldsLocalDir)
	if err := os.MkdirAll(local, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, w := range worlds {
		if err := os.WriteFile(filepath.Join(local, w.name+".db"), w.db, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(local, w.name+".fwl"), fwlBytes(37, w.name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backup.Archive(tree, dest); err != nil {
		t.Fatal(err)
	}
	return dest
}

// backupsDirFor is where a server's backup archives live in the test's data root.
func backupsDirFor(rt *Server, id string) string {
	return filepath.Join(instance.BackupsDir(rt.instances.Cfg.Data.Root), id)
}

// seedSourceServer adds inst-b, a stopped server with no container, which is all a source of
// backups needs to be, and a catalogue row for the archive at path, a backup of Dvalin.
func seedSourceServer(t *testing.T, rt *Server, db *store.DB, backupID, path string) {
	t.Helper()
	const id, world = "inst-b", "Dvalin"
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, 2556, 'Other', ?, 'x', ?, ?, ?)`,
		id, id, rt.instances.Cfg.Data.HostRoot+"/instances/"+id, world, "cp-"+id, store.Now(), store.Now())
	seed(
		t,
		db,
		`INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES (?, ?, ?, 1, 'x', ?, 'manual', 1, ?)`,
		backupID,
		id,
		path,
		world,
		store.Now(),
	)
}

// seedOperator adds a non-administrator who may import worlds into inst-a.
func seedOperator(t *testing.T, db *store.DB) *store.User {
	t.Helper()
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-op', 'opal', 'argon2id$stub', 'member', ?)`, store.Now())
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-op', 'inst-a', 'operator', '["world.import"]', ?)`, store.Now())
	return &store.User{ID: "u-op", Username: "opal", Role: store.RoleMember}
}

// grantOnSource gives u-op role on inst-b.
func grantOnSource(t *testing.T, db *store.DB, role string) {
	t.Helper()
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-op', 'inst-b', ?, '[]', ?)`, role, store.Now())
}

func postImportBackup(t *testing.T, body map[string]string) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, importBackupPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// errorDetail reads one field of the error envelope's details.
func errorDetail(t *testing.T, rec *httptest.ResponseRecorder, key string) any {
	t.Helper()
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &body)
	return body.Error.Details[key]
}

// worldDB reads the data half of inst-a's world, World.
func worldDB(t *testing.T, db *store.DB) []byte {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(worldsDirOf(t, db), instance.WorldsLocalDir, "World.db"))
	if err != nil {
		t.Fatalf("World.db: %v", err)
	}
	return got
}

// A world backed up on one server is installed on another under the other's world name, and
// the world it replaces is backed up first like any import.
func TestImportBackupFromAnotherServer(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedExistingWorld(t, db)
	archive := writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "inst-b"), "b.tar.gz"),
		archiveWorld{"Dvalin", bytes.Repeat([]byte("from b "), 300)})
	seedSourceServer(t, rt, db, "bk-1", archive)

	rec := as(rt, admin, postImportBackup(t, map[string]string{
		"source_instance_id": "inst-b", "backup_id": "bk-1",
	}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import = %s / %s", final.Status, deref(final.Error))
	}

	if got := worldDB(t, db); !bytes.HasPrefix(got, []byte("from b ")) {
		t.Error("the world on inst-a is not the one from inst-b's backup")
	}
	var snapshots int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT count(*) FROM backups WHERE instance_id = 'inst-a' AND trigger = ?`,
		store.TriggerPreImport).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Errorf("pre-import backups of inst-a = %d, %v, want 1", snapshots, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the source archive was consumed: %v", err)
	}
}

// What a caller may take a world from follows what they may see and download, and an invisible
// server is a 404 rather than a refusal that confirms it exists.
func TestImportBackupChecksAccessToTheSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		role string
		want int
	}{
		{"a viewer cannot download", "viewer", http.StatusForbidden},
		{"an operator can", "operator", http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, db, fake, _, _ := lifecycleWorld(t)
			seedInstance(t, rt, db, fake, "stopped")
			archive := writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "inst-b"), "b.tar.gz"),
				archiveWorld{"Dvalin", dbBytes()})
			seedSourceServer(t, rt, db, "bk-1", archive)
			op := seedOperator(t, db)
			grantOnSource(t, db, tc.role)

			rec := as(rt, op, postImportBackup(t, map[string]string{
				"source_instance_id": "inst-b", "backup_id": "bk-1",
			}))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}

	t.Run("an unseen server is a 404", func(t *testing.T) {
		t.Parallel()
		rt, db, fake, _, _ := lifecycleWorld(t)
		seedInstance(t, rt, db, fake, "stopped")
		archive := writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "inst-b"), "b.tar.gz"),
			archiveWorld{"Dvalin", dbBytes()})
		seedSourceServer(t, rt, db, "bk-1", archive)
		op := seedOperator(t, db)

		rec := as(rt, op, postImportBackup(t, map[string]string{
			"source_instance_id": "inst-b", "backup_id": "bk-1",
		}))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
		}
	})
}

// The target is held to the same rule as every world write: it must be stopped.
func TestImportBackupIntoARunningServerIsRefused(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "running")
	archive := writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "inst-b"), "b.tar.gz"),
		archiveWorld{"Dvalin", dbBytes()})
	seedSourceServer(t, rt, db, "bk-1", archive)

	rec := as(rt, admin, postImportBackup(t, map[string]string{
		"source_instance_id": "inst-b", "backup_id": "bk-1",
	}))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if c := fake.Get(containerID); c == nil || !c.Running {
		t.Error("the running server was stopped")
	}
}

// A backup names every world its server kept. Without a choice the catalogue row's world is
// the one wanted, and an archive with several and no row to say is a refusal that lists them.
func TestImportBackupWithSeveralWorlds(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	dir := backupsDirFor(rt, "gone")
	archive := writeBackupArchive(t, filepath.Join(dir, "two.tar.gz"),
		archiveWorld{"Dvalin", bytes.Repeat([]byte("dvalin "), 300)},
		archiveWorld{"Midgard", bytes.Repeat([]byte("midgard "), 300)})

	rec := as(rt, admin, postImportBackup(t, map[string]string{
		"source_instance_id": "gone", "filename": "two.tar.gz",
	}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no choice: status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	worlds, _ := errorDetail(t, rec, "worlds").([]any)
	if len(worlds) != 2 || worlds[0] != "Dvalin" || worlds[1] != "Midgard" {
		t.Errorf("details.worlds = %v, want [Dvalin Midgard]", worlds)
	}

	rec = as(rt, admin, postImportBackup(t, map[string]string{
		"source_instance_id": "gone", "filename": "two.tar.gz", "world": "Midgard",
	}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("with a choice: status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import = %s / %s", final.Status, deref(final.Error))
	}
	if got := worldDB(t, db); !bytes.HasPrefix(got, []byte("midgard ")) {
		t.Error("the world installed is not Midgard")
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the source archive was consumed: %v", err)
	}

	rec = as(rt, admin, postImportBackup(t, map[string]string{
		"source_instance_id": "gone", "filename": "two.tar.gz", "world": "Nope",
	}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown world: status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// A deleted server's archives are listed for administrators and only for archives nothing
// else claims: not a live server's, and not one still being written.
func TestOrphanedBackupsListsWhatADeletedServerLeft(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "inst-a"), "live.tar.gz"),
		archiveWorld{"World", dbBytes()})
	writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "gone"), "old.tar.gz"),
		archiveWorld{"Dvalin", dbBytes()})
	for _, name := range []string{"half.tar.gz.part", "old.tar.gz.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(backupsDirFor(rt, "gone"), name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/backups/orphaned", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Items []orphanedBackup `json:"items"`
	}
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].InstanceID != "gone" || page.Items[0].Filename != "old.tar.gz" {
		t.Errorf("items = %+v, want only gone/old.tar.gz", page.Items)
	}
	if page.Items[0].SizeBytes == 0 || page.Items[0].ModifiedAt == "" {
		t.Errorf("item = %+v lacks its size or time", page.Items[0])
	}

	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/backups/orphaned", http.NoBody)); rec.Code != http.StatusForbidden {
		t.Errorf("a non-administrator: status = %d, want 403", rec.Code)
	}
}

// An orphaned archive is found by matching the listing, so a name that is not in it cannot
// reach a file: not a live server's archive, not a traversal, not another user's say-so.
func TestImportBackupFromAnOrphanOnlyReachesListedArchives(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "gone"), "old.tar.gz"),
		archiveWorld{"Dvalin", bytes.Repeat([]byte("old "), 400)})
	writeBackupArchive(t, filepath.Join(backupsDirFor(rt, "inst-a"), "live.tar.gz"),
		archiveWorld{"World", dbBytes()})

	for _, tc := range []map[string]string{
		{"source_instance_id": "inst-a", "filename": "live.tar.gz"},
		{"source_instance_id": "gone", "filename": "../inst-a/live.tar.gz"},
		{"source_instance_id": "../inst-a", "filename": "live.tar.gz"},
		{"source_instance_id": "gone", "filename": "missing.tar.gz"},
	} {
		rec := as(rt, admin, postImportBackup(t, tc))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%v: status = %d, want 404 (%s)", tc, rec.Code, rec.Body)
		}
	}

	op := seedOperator(t, db)
	rec := as(rt, op, postImportBackup(t, map[string]string{
		"source_instance_id": "gone", "filename": "old.tar.gz",
	}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a non-administrator: status = %d, want 404 (%s)", rec.Code, rec.Body)
	}

	rec = as(rt, admin, postImportBackup(t, map[string]string{
		"source_instance_id": "gone", "filename": "old.tar.gz",
	}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a listed archive: status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("import = %s / %s", final.Status, deref(final.Error))
	}
	if got := worldDB(t, db); !bytes.HasPrefix(got, []byte("old ")) {
		t.Error("the world installed is not the orphan's")
	}
}

// A request has to name where the world comes from, and only one way.
func TestImportBackupRequiresOneSource(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	for _, tc := range []map[string]string{
		{},
		{"source_instance_id": "inst-b"},
		{"backup_id": "bk-1"},
		{"source_instance_id": "inst-b", "backup_id": "bk-1", "filename": "x.tar.gz"},
	} {
		rec := as(rt, admin, postImportBackup(t, tc))
		if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want a validation failure (%s)", tc, rec.Code, rec.Body)
		}
	}
}

// A source archive whose file is gone or damaged is reported, and the target is untouched.
func TestImportBackupWithAnUnusableArchiveLeavesTheTargetAlone(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedExistingWorld(t, db)
	broken := filepath.Join(backupsDirFor(rt, "inst-b"), "broken.tar.gz")
	if err := os.MkdirAll(filepath.Dir(broken), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedSourceServer(t, rt, db, "bk-broken", broken)
	seed(
		t,
		db,
		`INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES ('bk-gone', 'inst-b', ?, 1, 'x', 'Dvalin', 'manual', 1, ?)`,
		filepath.Join(backupsDirFor(rt, "inst-b"), "missing.tar.gz"),
		store.Now(),
	)

	for id, want := range map[string]int{
		"bk-broken": http.StatusUnprocessableEntity,
		"bk-gone":   http.StatusNotFound,
	} {
		rec := as(rt, admin, postImportBackup(t, map[string]string{
			"source_instance_id": "inst-b", "backup_id": id,
		}))
		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d (%s)", id, rec.Code, want, rec.Body)
		}
	}
	if got := worldDB(t, db); string(got) != "THE ORIGINAL WORLD" {
		t.Error("a refused import changed the target's world")
	}
}

// A budget that free space cut below the upload limit means the disk is what ran out, so an
// archive that expands past it is insufficient_disk, as it is for an upload.
func TestStageBackupWorldReportsALowDiskAsSuch(t *testing.T) {
	t.Parallel()
	archive := writeBackupArchive(t, filepath.Join(t.TempDir(), "b.tar.gz"),
		archiveWorld{"Dvalin", bytes.Repeat([]byte("x"), 64<<10)})

	_, err := stageBackupWorld(backupSource{path: archive}, "", t.TempDir(), 1<<10)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != errcode.InsufficientDisk {
		t.Errorf("err = %v, want insufficient_disk", err)
	}
}
