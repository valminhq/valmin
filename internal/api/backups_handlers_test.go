package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

const backupsPath = "/api/v1/instances/inst-a/backups"

// seededWorldName is the world seedInstance's row carries; every archive here is of it.
const seededWorldName = "World"

// seedArchive writes a file under backups/ and the catalogue row naming it, the way a
// finished backup job leaves the pair.
func seedArchive(
	t *testing.T, db *store.DB, root, id, trigger string, consistent bool, at time.Time,
) string {
	t.Helper()
	dir := filepath.Join(root, "backups", "inst-a")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".tar.gz")
	body := strings.Repeat("archive bytes ", 40)
	if err := os.WriteFile(path, []byte(body), 0o664); err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO backups
		(id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES (?, 'inst-a', ?, ?, 'abc123', ?, ?, ?, ?)`,
		id, path, len(body), seededWorldName, trigger, consistent, store.FormatTime(at))
	return path
}

func backupsWorld(t *testing.T) (rt *Router, db *store.DB, root string, admin, member *store.User) {
	t.Helper()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	return rt, db, rt.Supervisor().inst.Cfg.Data.Root, admin, member
}

// backupsPage is the list response, decoded far enough to assert order and the absence of a
// path.
type backupsPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

func listBackupsAs(t *testing.T, rt *Router, u *store.User, query string) backupsPage {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, backupsPath+query, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list backups = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page backupsPage
	decodeInto(t, rec, &page)
	return page
}

// Asserts the catalogue lists newest first, distinguishes a hot copy, and carries no
// filesystem path (11 §8.3, B12).
func TestListBackupsIsNewestFirstAndCarriesNoPath(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	now := time.Now().UTC()
	seedArchive(t, db, root, "b-old", store.TriggerManual, true, now.Add(-2*time.Hour))
	seedArchive(t, db, root, "b-new", store.TriggerScheduled, true, now)

	// A hot copy, oldest of the three. B12: an unlabelled one reads as the safe archive.
	seedArchive(t, db, root, "b-hot", store.TriggerManual, false, now.Add(-3*time.Hour))

	page := listBackupsAs(t, rt, admin, "")
	if len(page.Items) != 3 {
		t.Fatalf("listed %d archives, want 3", len(page.Items))
	}
	if page.Items[0]["id"] != "b-new" || page.Items[2]["id"] != "b-hot" {
		t.Errorf("archives are not newest first: %v", []any{
			page.Items[0]["id"], page.Items[1]["id"], page.Items[2]["id"],
		})
	}
	if page.Items[2]["consistent"] != false || page.Items[0]["consistent"] != true {
		t.Error("the list does not distinguish a hot copy from a quiesced archive (B12)")
	}
	for _, item := range page.Items {
		if _, ok := item["path"]; ok {
			t.Error("the list carries a raw filesystem path (11 §8.3)")
		}
		if item["filename"] == "" {
			t.Error("the list carries no filename, so a client cannot name a download")
		}
	}
}

func TestListBackupsPagesByCursor(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	now := time.Now().UTC()
	for i, id := range []string{"b-1", "b-2", "b-3"} {
		seedArchive(t, db, root, id, store.TriggerManual, true,
			now.Add(-time.Duration(i)*time.Hour))
	}

	first := listBackupsAs(t, rt, admin, "?limit=2")
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page = %d items, next_cursor %v; want 2 and a cursor",
			len(first.Items), first.NextCursor)
	}
	second := listBackupsAs(t, rt, admin, "?limit=2&cursor="+*first.NextCursor)
	if len(second.Items) != 1 {
		t.Fatalf("second page = %d items, want 1", len(second.Items))
	}
	if second.Items[0]["id"] != "b-3" {
		t.Errorf("second page starts at %v, want b-3", second.Items[0]["id"])
	}
	if second.NextCursor != nil {
		t.Error("the last page still offers a cursor; next_cursor null is the end (11 §4)")
	}
}

// Asserts a download serves the archive's own bytes under the name its row carries.
func TestDownloadServesTheArchiveNamedByTheRow(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	path := seedArchive(t, db, root, "b-1", store.TriggerManual, true, time.Now().UTC())
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, backupsPath+"/b-1/download", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec.Body.String() != string(want) {
		t.Error("the downloaded bytes are not the archive's")
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "b-1.tar.gz") {
		t.Errorf("Content-Disposition = %q, want the archive's own filename", got)
	}
	if got := rec.Header().Get("Content-Length"); got == "" {
		t.Error("no Content-Length; 11 §8.3 sets one where it is known")
	}
}

// Asserts a row whose file has gone answers 404 rather than a truncated 200.
func TestDownloadOfAMissingFileIsNotFound(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	path := seedArchive(t, db, root, "b-1", store.TriggerManual, true, time.Now().UTC())
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, backupsPath+"/b-1/download", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download of a vanished archive = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// Asserts an archive id from another instance is 404 and is left untouched (ADR-038).
func TestBackupOfAnotherInstanceIsNotFound(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-b', 'inst-b', 'stopped', ?, 2461, 'B', 'World', '', 'cp-inst-b', ?, ?)`,
		filepath.Join(root, "instances", "inst-b"), store.Now(), store.Now())
	path := filepath.Join(root, "backups", "inst-b", "b-other.tar.gz")
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("another instance's world"), 0o664); err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO backups
		(id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES ('b-other', 'inst-b', ?, 24, 'abc', 'World', 'manual', 1, ?)`, path, store.Now())

	for _, route := range []struct{ method, target string }{
		{http.MethodGet, backupsPath + "/b-other/download"},
		{http.MethodDelete, backupsPath + "/b-other"},
		{http.MethodPost, backupsPath + "/b-other/restore"},
	} {
		rec := as(rt, admin, httptest.NewRequest(route.method, route.target, http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", route.method, route.target, rec.Code)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the other instance's archive was removed by a request naming inst-a")
	}
}

func TestDeleteRemovesTheFileAndTheRow(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	path := seedArchive(t, db, root, "b-1", store.TriggerManual, true, time.Now().UTC())

	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete, backupsPath+"/b-1", http.NoBody))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the archive file survived its catalogue row")
	}
	if page := listBackupsAs(t, rt, admin, ""); len(page.Items) != 0 {
		t.Errorf("the catalogue still lists %d archives after the delete", len(page.Items))
	}
}

// Asserts a row whose file is already gone still deletes, so no row is unremovable.
func TestDeleteToleratesAnAlreadyMissingFile(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	path := seedArchive(t, db, root, "b-1", store.TriggerManual, true, time.Now().UTC())
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete, backupsPath+"/b-1", http.NoBody))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete of a rowless archive = %d, want 204 (%s)", rec.Code, rec.Body)
	}
}

// Asserts 09 §3's role sets at the three routes: a viewer lists, and can neither download
// nor delete.
func TestBackupRoutesEnforceTheirActions(t *testing.T) {
	rt, db, root, _, member := backupsWorld(t)
	seedArchive(t, db, root, "b-1", store.TriggerManual, true, time.Now().UTC())

	if page := listBackupsAs(t, rt, member, ""); len(page.Items) != 1 {
		t.Errorf("a viewer listed %d archives, want 1 (backups.list)", len(page.Items))
	}
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, backupsPath + "/b-1/download"},
		{http.MethodDelete, backupsPath + "/b-1"},
	} {
		rec := as(rt, member, httptest.NewRequest(tc.method, tc.target, http.NoBody))
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", tc.method, tc.target, rec.Code)
		}
	}
}

// Asserts a caller with no grant cannot learn the instance's archives exist (ADR-038, D5).
func TestBackupRoutesAreNotFoundWithoutAGrant(t *testing.T) {
	rt, db, root, _, _ := backupsWorld(t)
	seedArchive(t, db, root, "b-1", store.TriggerManual, true, time.Now().UTC())
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-none', 'nan', 'argon2id$stub', 'member', ?)`, store.Now())
	stranger := &store.User{ID: "u-none", Username: "nan", Role: store.RoleMember}

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, backupsPath},
		{http.MethodGet, backupsPath + "/b-1/download"},
		{http.MethodDelete, backupsPath + "/b-1"},
	} {
		rec := as(rt, stranger, httptest.NewRequest(tc.method, tc.target, http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Errorf("ungranted %s %s = %d, want 404 — 403 would confirm it exists",
				tc.method, tc.target, rec.Code)
		}
	}
}

// Asserts no handler here builds a path at all. D13 with teeth: an archive's path comes from
// its catalogue row, and one built from the request is a traversal bug.
func TestNoBackupHandlerJoinsARequestValue(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "backups.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		// toBackupView shortens the row's own path with filepath.Base rather than building one.
		if !ok || fn.Name.Name == "toBackupView" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Join" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "filepath" {
				t.Errorf("%s calls filepath.Join; a backup path comes from the catalogue row, "+
					"never from the request (D13, 11 §8.3)", fn.Name.Name)
			}
			return true
		})
	}
}
