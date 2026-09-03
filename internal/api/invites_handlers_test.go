package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// seedTestInstance inserts the minimal row a grant or invite can reference, mirroring
// permissions_test.go's world() helper.
func seedTestInstance(t *testing.T, db *store.DB, id string, basePort int) {
	t.Helper()
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, ?, ?, 'v1.k.n.ct', ?, ?, ?)`,
		id, id, "/srv/valmin/instances/"+id, basePort,
		"Server "+id, "World"+id, "cp-"+id, store.Now(), store.Now())
}

func TestIssueAndRedeemInvite(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)

	issue := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites",
		jsonBody(t, map[string]any{})), admin))
	if issue.Code != http.StatusCreated {
		t.Fatalf("issue = %d (%s), want 201", issue.Code, issue.Body)
	}
	var issued struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	decodeInto(t, issue, &issued)
	if issued.Token == "" || issued.URL == "" {
		t.Fatalf("issued = %+v", issued)
	}

	redeem := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/invites/"+issued.Token+"/redeem",
		jsonBody(t, map[string]string{"username": "newbie", "password": "a-fine-password"})))
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem = %d (%s), want 200", redeem.Code, redeem.Body)
	}
	if cookieValue(redeem, "valmin_session") == "" {
		t.Error("redeem did not log the new user in")
	}
}

func TestIssueWithAGrant(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	seedTestInstance(t, db, "inst-a", 2456)

	issue := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites", jsonBody(t, map[string]any{
		"instance_id": "inst-a", "grant_role": "viewer", "grant_perms": []string{"backups.create"},
	})), admin))
	if issue.Code != http.StatusCreated {
		t.Fatalf("issue = %d (%s)", issue.Code, issue.Body)
	}
	var issued struct{ Token string }
	decodeInto(t, issue, &issued)

	redeem := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/invites/"+issued.Token+"/redeem",
		jsonBody(t, map[string]string{"username": "newbie", "password": "a-fine-password"})))
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem = %d (%s)", redeem.Code, redeem.Body)
	}

	member := redeem
	caps := send(rt, authenticated(httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-a/capabilities", http.NoBody), member))
	if caps.Code != http.StatusOK {
		t.Fatalf("capabilities = %d (%s), want the redeemed grant to already be live", caps.Code, caps.Body)
	}
}

// TestIssueRejectsANeverGrantableCapability is 09 §3.3 at the point of request: an invite
// must not be the loophole that smuggles instance.delete into someone's perms.
func TestIssueRejectsANeverGrantableCapability(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	seedTestInstance(t, db, "inst-a", 2456)

	rec := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites", jsonBody(t, map[string]any{
		"instance_id": "inst-a", "grant_role": "viewer", "grant_perms": []string{"instance.delete"},
	})), admin))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestIssueRejectsAnInstanceWithoutARole(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	seedTestInstance(t, db, "inst-a", 2456)

	rec := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites", jsonBody(t, map[string]any{
		"instance_id": "inst-a",
	})), admin))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

// TestRedeemInvalidResponsesAreByteIdentical is the anti-enumeration
// criterion: an expired invite and a never-existed code must not be distinguishable.
func TestRedeemInvalidResponsesAreByteIdentical(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)

	issue := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites",
		jsonBody(t, map[string]any{})), admin))
	var issued struct{ Token string }
	decodeInto(t, issue, &issued)

	firstRedeem := send(rt, httptest.NewRequest(http.MethodPost,
		"/api/v1/invites/"+issued.Token+"/redeem",
		jsonBody(t, map[string]string{"username": "first", "password": "a-fine-password"})))
	if firstRedeem.Code != http.StatusOK {
		t.Fatalf("seeding the redeemed invite: %d (%s)", firstRedeem.Code, firstRedeem.Body)
	}

	redeemBody := jsonBody(t, map[string]string{"username": "second", "password": "a-fine-password"})
	alreadyRedeemed := send(rt, httptest.NewRequest(http.MethodPost,
		"/api/v1/invites/"+issued.Token+"/redeem", redeemBody))

	neverExisted := send(rt, httptest.NewRequest(http.MethodPost,
		"/api/v1/invites/totally-made-up-code/redeem",
		jsonBody(t, map[string]string{"username": "third", "password": "a-fine-password"})))

	if alreadyRedeemed.Code != http.StatusGone || neverExisted.Code != http.StatusGone {
		t.Fatalf("status = %d, %d, want 410, 410", alreadyRedeemed.Code, neverExisted.Code)
	}
	strip := func(rec *httptest.ResponseRecorder) string {
		var body map[string]any
		decodeInto(t, rec, &body)
		if e, ok := body["error"].(map[string]any); ok {
			delete(e, "request_id")
		}
		raw, _ := json.Marshal(body)
		return string(raw)
	}
	if strip(alreadyRedeemed) != strip(neverExisted) {
		t.Errorf("responses differ: %s vs %s", strip(alreadyRedeemed), strip(neverExisted))
	}
}

func TestRevokeInvite(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	issue := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites",
		jsonBody(t, map[string]any{})), admin))
	var issued struct{ Token string }
	decodeInto(t, issue, &issued)

	list := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/invites", http.NoBody), admin))
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodeInto(t, list, &page)
	if len(page.Items) != 1 {
		t.Fatalf("list has %d invites, want 1", len(page.Items))
	}

	revoke := send(rt, authenticated(httptest.NewRequest(http.MethodDelete,
		"/api/v1/invites/"+page.Items[0].ID, http.NoBody), admin))
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", revoke.Code)
	}

	redeem := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/invites/"+issued.Token+"/redeem",
		jsonBody(t, map[string]string{"username": "x", "password": "a-fine-password"})))
	if redeem.Code != http.StatusGone {
		t.Errorf("redeeming a revoked invite = %d, want 410", redeem.Code)
	}
}

// TestInvitesManageIsNeverGrantable mirrors the users.manage test: a member cannot reach
// any invite-management route, 404 not 403 (D2).
func TestInvitesManageIsNeverGrantable(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
		"username": "mel", "password": "a-fine-password", "role": "member",
	})), admin))
	member := loginAs(t, rt, "mel", "a-fine-password")

	rec := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites",
		jsonBody(t, map[string]any{})), member))
	if rec.Code != http.StatusNotFound {
		t.Errorf("issue as a member = %d, want 404", rec.Code)
	}
}
