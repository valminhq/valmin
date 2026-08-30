package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// getList performs the GET and hands back both the body and the ETag, which is what a real
// client holds between a read and its write.
func getList(t *testing.T, rt *Router, u *store.User, path string) (ids []string, etag string) {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (%s)", path, rec.Code, rec.Body)
	}
	var body playerListView
	decodeInto(t, rec, &body)
	return body.IDs, rec.Header().Get("ETag")
}

func putList(t *testing.T, rt *Router, u *store.User, path, etag string, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(playerListView{IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	return as(rt, u, req)
}

func listPath(list string) string { return "/api/v1/instances/inst-a/" + list }

// listOnDisk reads the file the game would read, not the API's view of it.
func listOnDisk(t *testing.T, db *store.DB, name instance.PlayerList) string {
	t.Helper()
	var dataDir string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT data_dir FROM instances WHERE id = 'inst-a'`).Scan(&dataDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dataDir + "/worlds/" + string(name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestPlayerListStartsEmptyAndRoundTrips: the game creates none of the three files, so an
// instance that has never had one must still be readable and writable.
func TestPlayerListStartsEmptyAndRoundTrips(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	for _, list := range []string{"admins", "bans", "permitted"} {
		ids, etag := getList(t, rt, admin, listPath(list))
		if len(ids) != 0 {
			t.Errorf("%s starts as %q, want empty", list, ids)
		}
		if etag == "" {
			t.Errorf("%s returned no ETag; PUT would be unreachable", list)
		}

		rec := putList(t, rt, admin, listPath(list), etag, []string{"Steam_76561198000000000"})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d, want 200 (%s)", list, rec.Code, rec.Body)
		}
		back, _ := getList(t, rt, admin, listPath(list))
		if len(back) != 1 || back[0] != "Steam_76561198000000000" {
			t.Errorf("%s read back as %q", list, back)
		}
	}

	if got := listOnDisk(t, db, instance.AdminList); got != "Steam_76561198000000000\n" {
		t.Errorf("adminlist.txt on disk = %q, want one id and a trailing newline", got)
	}
}

// TestPlayerListStaleWriteIsRejectedAndChangesNothing is 11 §1.1's whole reason for existing:
// two co-admins editing from two browsers is 01 §2's primary user, and silently keeping the
// second write is data loss with a plausible cover story.
func TestPlayerListStaleWriteIsRejectedAndChangesNothing(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	_, etag := getList(t, rt, admin, listPath("admins"))

	// The first co-admin writes. Their ETag is now the current one.
	if rec := putList(t, rt, admin, listPath("admins"), etag, []string{"Steam_1"}); rec.Code != http.StatusOK {
		t.Fatalf("first write = %d (%s)", rec.Code, rec.Body)
	}

	// The second co-admin, holding the ETag they loaded *before* that write, tries to save.
	rec := putList(t, rt, admin, listPath("admins"), etag, []string{"Steam_2"})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale write = %d, want 412 (%s)", rec.Code, rec.Body)
	}
	if got := listOnDisk(t, db, instance.AdminList); got != "Steam_1\n" {
		t.Errorf("the file changed on a rejected write: %q", got)
	}
}

// TestPlayerListRequiresIfMatch: a client that never sends the header is broken, which is a
// different thing from losing a race — 400 naming the header, not 412.
func TestPlayerListRequiresIfMatch(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := putList(t, rt, admin, listPath("admins"), "", []string{"Steam_1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with no If-Match = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if got := listOnDisk(t, db, instance.AdminList); got != "" {
		t.Errorf("the file was written despite a missing precondition: %q", got)
	}
}

// TestPlayerListNormalisesRatherThanWritingAsIs is 03 §4's strict format, end to end.
func TestPlayerListNormalisesRatherThanWritingAsIs(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	_, etag := getList(t, rt, admin, listPath("admins"))
	rec := putList(t, rt, admin, listPath("admins"), etag, []string{"  Steam_1  ", "", "76561198000000000 "})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := listOnDisk(t, db, instance.AdminList); got != "Steam_1\n76561198000000000\n" {
		t.Errorf("on disk = %q, want trimmed ids with the blank row dropped", got)
	}
}

// TestPlayerListRejectsEntriesThatWouldSilentlyFail — with per-row field errors, since a
// list is edited as one document and one error at a time would be miserable.
func TestPlayerListRejectsEntriesThatWouldSilentlyFail(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	_, etag := getList(t, rt, admin, listPath("admins"))
	rec := putList(t, rt, admin, listPath("admins"), etag,
		[]string{"Steam_1", "# my friends", "7656 1198"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d, want 422 (%s)", rec.Code, rec.Body)
	}

	var envelope struct {
		Error struct {
			Details struct {
				Fields []struct {
					Field string `json:"field"`
				} `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &envelope)
	if len(envelope.Error.Details.Fields) != 2 {
		t.Fatalf("fields = %+v, want both bad rows reported at once", envelope.Error.Details.Fields)
	}
	if envelope.Error.Details.Fields[0].Field != "ids.1" {
		t.Errorf("field = %q, want ids.1 so the UI can highlight the row",
			envelope.Error.Details.Fields[0].Field)
	}
	if got := listOnDisk(t, db, instance.AdminList); got != "" {
		t.Errorf("a rejected list was partially written: %q", got)
	}
}

// TestPlayerListNeedsPlayersManage is D1 and D2 in one: a viewer holds no players action, and
// an instance the caller cannot see does not exist.
func TestPlayerListNeedsPlayersManage(t *testing.T) {
	rt, db, fake, _, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	// seedInstance grants the member `viewer` on inst-a, which 09 §3.1 gives no
	// players-shaped capability.
	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, listPath("admins"), http.NoBody)); rec.Code != http.StatusForbidden {
		t.Errorf("viewer GET = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := putList(t, rt, member, listPath("admins"), `"x"`, []string{"Steam_1"}); rec.Code != http.StatusForbidden {
		t.Errorf("viewer PUT = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	// An instance with no grant at all is 404, never 403 (ADR-038).
	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-nope/admins", http.NoBody)); rec.Code != http.StatusNotFound {
		t.Errorf("unseen instance = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestPlayerListWriteIsAudited: an admin list is who can ban whom, so a change to it belongs
// in the permanent record (09 §4).
func TestPlayerListWriteIsAudited(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	_, etag := getList(t, rt, admin, listPath("bans"))
	if rec := putList(t, rt, admin, listPath("bans"), etag, []string{"Steam_1"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body)
	}
	// The three lists are separate files: writing bans must not touch admins.
	if got := listOnDisk(t, db, instance.BannedList); got != "Steam_1\n" {
		t.Errorf("bannedlist.txt = %q, want the id just written", got)
	}
	if got := listOnDisk(t, db, instance.AdminList); got != "" {
		t.Errorf("adminlist.txt = %q, want untouched by a ban write", got)
	}

	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_log WHERE instance_id = 'inst-a' AND action LIKE 'instances.players.%'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("audit rows = %d, want 1", count)
	}
}
