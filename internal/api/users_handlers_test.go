package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// bootstrappedRouter returns a router with one admin already logged in — the state
// almost every users/invites test starts from.
func bootstrappedRouter(t *testing.T) (rt *Router, db *store.DB, admin *httptest.ResponseRecorder) {
	t.Helper()
	rt, db = pendingRouter(t)
	token := bootstrapToken(t, db)
	admin = send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))
	if admin.Code != http.StatusOK {
		t.Fatalf("setup: %d (%s)", admin.Code, admin.Body)
	}
	return rt, db, admin
}

func loginAs(t *testing.T, rt *Router, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	rec := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": username, "password": password,
	})))
	if rec.Code != http.StatusOK {
		t.Fatalf("login as %s: %d (%s)", username, rec.Code, rec.Body)
	}
	return rec
}

func TestCreateListAndDeleteUser(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)

	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin),
	)
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s), want 201", create.Code, create.Body)
	}
	var created struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	decodeInto(t, create, &created)
	if created.Username != "bea" || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	list := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/users", http.NoBody), admin))
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d", list.Code)
	}
	var page struct {
		Items []struct{ Username string } `json:"items"`
	}
	decodeInto(t, list, &page)
	if len(page.Items) != 2 { // ada + bea
		t.Errorf("list has %d users, want 2", len(page.Items))
	}

	del := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+created.ID, http.NoBody), admin),
	)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", del.Code)
	}
	missing := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+created.ID, http.NoBody), admin),
	)
	if missing.Code != http.StatusNotFound {
		t.Errorf("deleting again = %d, want 404", missing.Code)
	}
}

// TestUsersManageIsNeverGrantable is 09 §3.3 at the HTTP layer: a member — who by
// construction holds no grant naming users.manage, since it cannot be granted at all —
// gets 404, not 403 (D2), on every route in this file.
func TestUsersManageIsNeverGrantable(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
		"username": "mel", "password": "a-fine-password", "role": "member",
	})), admin))
	member := loginAs(t, rt, "mel", "a-fine-password")

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/users", http.NoBody),
		httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "x", "password": "a-fine-password", "role": "member",
		})),
	} {
		rec := send(rt, authenticated(req, member))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as a member = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
}

// TestDisablingAUserRevokesTheirSessions is 10 §4.1's revocation rule reaching a live
// connection, exercised through the real PATCH handler.
func TestDisablingAUserRevokesTheirSessions(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "mel", "password": "a-fine-password", "role": "member",
		})), admin),
	)
	var created struct{ ID string }
	decodeInto(t, create, &created)

	member := loginAs(t, rt, "mel", "a-fine-password")
	if rec := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody), member),
	); rec.Code != http.StatusOK {
		t.Fatalf("member's session is not live before disabling: %d", rec.Code)
	}

	patch := send(rt, authenticated(httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+created.ID,
		jsonBody(t, map[string]bool{"disabled": true})), admin))
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH = %d (%s)", patch.Code, patch.Body)
	}

	after := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody), member))
	if after.Code != http.StatusUnauthorized {
		t.Errorf("member's session after disabling = %d, want 401", after.Code)
	}
}

func TestPasswordResetIssuesAWorkingPassword(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "mel", "password": "a-fine-password", "role": "member",
		})), admin),
	)
	var created struct{ ID string }
	decodeInto(t, create, &created)

	reset := send(rt, authenticated(httptest.NewRequest(http.MethodPost,
		"/api/v1/users/"+created.ID+"/password/reset", http.NoBody), admin))
	if reset.Code != http.StatusOK {
		t.Fatalf("reset = %d (%s)", reset.Code, reset.Body)
	}
	var got struct{ Password string }
	decodeInto(t, reset, &got)
	if got.Password == "" {
		t.Fatal("reset returned no password")
	}

	if rec := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": "mel", "password": got.Password,
	}))); rec.Code != http.StatusOK {
		t.Errorf("the reset password does not work: %d (%s)", rec.Code, rec.Body)
	}
}

func TestCreateUserRejectsADuplicateUsername(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	body := map[string]string{"username": "ada", "password": "another-password", "role": "member"}
	rec := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, body)), admin))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := errCode(t, rec); got != "name_taken" {
		t.Errorf("code = %q, want name_taken", got)
	}
}
