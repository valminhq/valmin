package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRoleResetAndDeleteRevokeSessions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Router, *httptest.ResponseRecorder, string) *httptest.ResponseRecorder
	}{
		{
			name: "role change",
			mutate: func(t *testing.T, rt *Router, admin *httptest.ResponseRecorder, id string) *httptest.ResponseRecorder {
				t.Helper()
				return send(rt, authenticated(httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+id,
					jsonBody(t, map[string]string{"role": "admin"})), admin))
			},
		},
		{
			name: "password reset",
			mutate: func(t *testing.T, rt *Router, admin *httptest.ResponseRecorder, id string) *httptest.ResponseRecorder {
				t.Helper()
				return send(rt, authenticated(httptest.NewRequest(http.MethodPost,
					"/api/v1/users/"+id+"/password/reset", http.NoBody), admin))
			},
		},
		{
			name: "deletion",
			mutate: func(t *testing.T, rt *Router, admin *httptest.ResponseRecorder, id string) *httptest.ResponseRecorder {
				t.Helper()
				return send(rt, authenticated(httptest.NewRequest(http.MethodDelete,
					"/api/v1/users/"+id, http.NoBody), admin))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, admin := bootstrappedRouter(t)
			create := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users",
				jsonBody(t, map[string]string{
					"username": "mel", "password": "a-fine-password", "role": "member",
				})), admin))
			var user struct{ ID string }
			decodeInto(t, create, &user)
			member := loginAs(t, rt, "mel", "a-fine-password")

			if rec := tt.mutate(t, rt, admin, user.ID); rec.Code < 200 || rec.Code >= 300 {
				t.Fatalf("mutation = %d (%s)", rec.Code, rec.Body)
			}
			after := send(rt, authenticated(httptest.NewRequest(http.MethodGet,
				"/api/v1/auth/me", http.NoBody), member))
			if after.Code != http.StatusUnauthorized {
				t.Errorf("old session after %s = %d, want 401", tt.name, after.Code)
			}
		})
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

func TestUserMutationsWriteCredentialFreeAudits(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	created := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users",
		jsonBody(t, map[string]string{
			"username": "mel", "password": "initial-secret", "role": "member",
		})), admin))
	var user struct{ ID string }
	decodeInto(t, created, &user)

	updated := send(rt, authenticated(httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+user.ID,
		jsonBody(t, map[string]any{"role": "admin", "disabled": false})), admin))
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", updated.Code, updated.Body)
	}
	reset := send(rt, authenticated(httptest.NewRequest(http.MethodPost,
		"/api/v1/users/"+user.ID+"/password/reset", http.NoBody), admin))
	if reset.Code != http.StatusOK {
		t.Fatalf("reset = %d (%s)", reset.Code, reset.Body)
	}
	var credential struct{ Password string }
	decodeInto(t, reset, &credential)
	deleted := send(rt, authenticated(httptest.NewRequest(http.MethodDelete,
		"/api/v1/users/"+user.ID, http.NoBody), admin))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", deleted.Code, deleted.Body)
	}

	rows, err := db.Reader.QueryContext(t.Context(), `
		SELECT action, detail FROM audit_log WHERE action LIKE 'users.%' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actions []string
	var details strings.Builder
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		details.WriteString(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(actions, ","); got != "users.create,users.update,users.password.reset,users.delete" {
		t.Errorf("audit actions = %q", got)
	}
	if strings.Contains(details.String(), "initial-secret") || strings.Contains(details.String(), credential.Password) {
		t.Errorf("audit details contain a plaintext credential: %s", details.String())
	}
}

// TestOwnerRefusesDemotionDisableAndDeletion is 09 §2 over HTTP. The refusal is 403, not
// 404: the caller can see the account, so ADR-038's hidden-resource rule does not apply.
func TestOwnerRefusesDemotionDisableAndDeletion(t *testing.T) {
	tests := []struct {
		name    string
		request func(*testing.T, string) *http.Request
	}{
		{"demote", func(t *testing.T, id string) *http.Request {
			return httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+id,
				jsonBody(t, map[string]string{"role": "member"}))
		}},
		{"disable", func(t *testing.T, id string) *http.Request {
			return httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+id,
				jsonBody(t, map[string]bool{"disabled": true}))
		}},
		{"delete", func(t *testing.T, id string) *http.Request {
			return httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+id, http.NoBody)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, admin := bootstrappedRouter(t)
			owner := listUsers(t, rt, admin)[0]
			if !owner.Owner {
				t.Fatalf("the bootstrap account is not flagged as the owner")
			}

			rec := send(rt, authenticated(tc.request(t, owner.ID), admin))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s the owner = %d (%s), want 403", tc.name, rec.Code, rec.Body)
			}

			after, err := db.UserByID(t.Context(), owner.ID)
			if err != nil || after == nil {
				t.Fatalf("owner after a refused %s = %v, %v", tc.name, after, err)
			}
			if after.Role != store.RoleAdmin || after.Disabled {
				t.Fatalf("owner after a refused %s is %s, disabled=%v", tc.name, after.Role, after.Disabled)
			}

			// The caller keeps working: a refusal must not have taken their session with it.
			if me := send(rt, authenticated(httptest.NewRequest(
				http.MethodGet, "/api/v1/auth/me", http.NoBody), admin)); me.Code != http.StatusOK {
				t.Fatalf("the owner's session after a refused %s = %d", tc.name, me.Code)
			}
		})
	}
}

func listUsers(t *testing.T, rt *Router, admin *httptest.ResponseRecorder) []store.User {
	t.Helper()
	rec := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/users", http.NoBody), admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("list users = %d (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Items []store.User `json:"items"`
	}
	decodeInto(t, rec, &page)
	return page.Items
}
