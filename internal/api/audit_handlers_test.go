package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

type auditPage struct {
	Items []struct {
		ID         string  `json:"id"`
		UserID     *string `json:"user_id"`
		Actor      *string `json:"actor"`
		InstanceID *string `json:"instance_id"`
		Instance   *string `json:"instance"`
		Action     string  `json:"action"`
		Detail     *string `json:"detail"`
		CreatedAt  string  `json:"created_at"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func readAudit(t *testing.T, rt *Router, session *httptest.ResponseRecorder, query string) auditPage {
	t.Helper()
	rec := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, http.NoBody), session))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /audit%s = %d (%s)", query, rec.Code, rec.Body)
	}
	var page auditPage
	decodeInto(t, rec, &page)
	return page
}

// seedAuditRow writes one row directly, so a test can pin created_at and name an actor or
// instance that does not exist.
func seedAuditRow(t *testing.T, db *store.DB, id, userID, instanceID, action, createdAt string) {
	t.Helper()
	var user, instance any
	if userID != "" {
		user = userID
	}
	if instanceID != "" {
		instance = instanceID
	}
	if _, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO audit_log (id, user_id, instance_id, action, detail, ip, created_at)
		VALUES (?, ?, ?, ?, '{}', '127.0.0.1', ?)`,
		id, user, instance, action, createdAt); err != nil {
		t.Fatal(err)
	}
}

func TestAuditIsInvisibleToAMember(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin),
	)
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", create.Code, create.Body)
	}
	member := loginAs(t, rt, "bea", "another-password")

	for _, query := range []string{"", "?action=users.create", "?user_id=whoever"} {
		rec := send(
			rt,
			authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, http.NoBody), member),
		)
		if rec.Code != http.StatusNotFound {
			t.Errorf("member GET /audit%s = %d, want 404", query, rec.Code)
		}
	}
}

// TestAuditPagesTiedTimestampsWithoutDuplicates is ADR-035's reason for the compound key:
// rows written in the same burst share created_at, and a page boundary that only ordered by
// time would repeat or skip one.
func TestAuditPagesTiedTimestampsWithoutDuplicates(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	const tied = "2026-01-02T03:04:05.000000000Z"
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		seedAuditRow(t, db, id, "", "", "tied.event", tied)
	}

	seen := map[string]bool{}
	query := "?limit=2&action=tied.event"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
		page := readAudit(t, rt, admin, query)
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("row %s appeared on two pages", item.ID)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		query = "?limit=2&action=tied.event&cursor=" + *page.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("paged over %d rows, want 5", len(seen))
	}
}

func TestAuditRejectsMalformedCursorsAndUnknownFilters(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)

	for name, query := range map[string]string{
		"malformed cursor": "?cursor=not-base64!!",
		"unknown filter":   "?detail=password",
		"bad limit":        "?limit=zero",
	} {
		rec := send(
			rt,
			authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, http.NoBody), admin),
		)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d (%s), want 400", name, rec.Code, rec.Body)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		decodeInto(t, rec, &envelope)
		if envelope.Error.Code != "invalid_parameter" {
			t.Errorf("%s: code %q, want invalid_parameter", name, envelope.Error.Code)
		}
	}
}

// TestAuditOutlivesItsSubjects is the point of the table: deleting the user or the instance
// an entry describes leaves the entry readable, with the ids intact and the names null.
func TestAuditOutlivesItsSubjects(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin),
	)
	var created struct {
		ID string `json:"id"`
	}
	decodeInto(t, create, &created)

	seedAuditRow(t, db, "gone", created.ID, "no-such-instance", "instances.delete", store.Now())

	del := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+created.ID, http.NoBody), admin),
	)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", del.Code, del.Body)
	}

	page := readAudit(t, rt, admin, "?action=instances.delete")
	if len(page.Items) != 1 {
		t.Fatalf("audit rows for a deleted user = %d, want 1", len(page.Items))
	}
	row := page.Items[0]
	if row.UserID == nil || *row.UserID != created.ID {
		t.Errorf("user_id = %v, want the deleted user's id", row.UserID)
	}
	if row.Actor != nil {
		t.Errorf("actor = %v, want null once the account is gone", *row.Actor)
	}
	if row.InstanceID == nil || *row.InstanceID != "no-such-instance" {
		t.Errorf("instance_id = %v, want the unresolvable id", row.InstanceID)
	}
	if row.Instance != nil {
		t.Errorf("instance = %v, want null", *row.Instance)
	}
}

// TestAuditNeverCarriesACredential guards 11 §8.3: the audit detail is written from the
// request, and a canary password or invite token must not survive into the trail.
func TestAuditNeverCarriesACredential(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	const canary = "canary-Ac1d-Rain-9182"

	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": canary, "role": "member",
		})), admin),
	)
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", create.Code, create.Body)
	}
	issued := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites", jsonBody(t, map[string]any{
			"instance_id": nil, "grant_role": nil, "grant_perms": []string{},
		})), admin),
	)
	if issued.Code != http.StatusCreated && issued.Code != http.StatusOK {
		t.Fatalf("issue invite = %d (%s)", issued.Code, issued.Body)
	}
	var invite struct {
		Token string `json:"token"`
	}
	decodeInto(t, issued, &invite)

	rec := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit", http.NoBody), admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, canary) {
		t.Error("the audit trail carries a password")
	}
	if invite.Token != "" && strings.Contains(body, invite.Token) {
		t.Error("the audit trail carries an invite token")
	}
	if !strings.Contains(body, "users.create") {
		t.Fatalf("no users.create row in %s", body)
	}
}
