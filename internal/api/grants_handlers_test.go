package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

type grantChangeRecorder struct {
	calls [][2]string
}

func (r *grantChangeRecorder) GrantChanged(_ context.Context, userID, instanceID string) {
	r.calls = append(r.calls, [2]string{userID, instanceID})
}

func grantRequest(t *testing.T, method, path, condition string, body any) *http.Request {
	t.Helper()
	var raw bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&raw).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &raw)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if condition == "*" {
		req.Header.Set("If-None-Match", "*")
	} else if condition != "" {
		req.Header.Set("If-Match", condition)
	}
	return req
}

func TestGrantCreateReplaceAndDeleteAreConditional(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-other', 'bea', 'argon2id$stub', 'member', ?)`, store.Now())
	path := "/api/v1/instances/inst-a/grants/u-other"

	missing := as(rt, admin, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("absent GET = %d, want 404", missing.Code)
	}

	created := as(rt, admin, grantRequest(t, http.MethodPut, path, "*", map[string]any{
		"role": "viewer", "perms": []string{"config.raw"},
	}))
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", created.Code, created.Body)
	}
	firstETag := created.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("create returned no ETag")
	}

	allowed, err := authz.New(db).Allowed(t.Context(), &store.User{ID: "u-other", Role: store.RoleMember}, "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(allowed))
	for _, action := range allowed {
		names = append(names, action.String())
	}
	if !slices.Contains(names, "config.raw") || !slices.Contains(names, "config.edit") {
		t.Errorf("config.raw did not imply config.edit: %v", names)
	}

	stale := as(rt, admin, grantRequest(t, http.MethodPut, path, `"stale"`, map[string]any{
		"role": "operator", "perms": []string{},
	}))
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale replace = %d, want 412 (%s)", stale.Code, stale.Body)
	}

	replaced := as(rt, admin, grantRequest(t, http.MethodPut, path, firstETag, map[string]any{
		"role": "operator", "perms": []string{},
	}))
	if replaced.Code != http.StatusOK {
		t.Fatalf("replace = %d, want 200 (%s)", replaced.Code, replaced.Body)
	}
	secondETag := replaced.Header().Get("ETag")
	if secondETag == "" || secondETag == firstETag {
		t.Fatalf("replacement ETag = %q, first = %q", secondETag, firstETag)
	}

	staleDelete := as(rt, admin, grantRequest(t, http.MethodDelete, path, firstETag, nil))
	if staleDelete.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale delete = %d, want 412", staleDelete.Code)
	}
	deleted := as(rt, admin, grantRequest(t, http.MethodDelete, path, secondETag, nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (%s)", deleted.Code, deleted.Body)
	}

	var audits int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_log
		WHERE instance_id = 'inst-a' AND user_id = 'u-admin' AND action LIKE 'instances.grants.%'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 3 {
		t.Errorf("audit rows = %d, want create, replace, and delete only", audits)
	}
}

func TestGrantCollectionOwnsTheCapabilityVocabulary(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-a/grants", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Roles []struct {
			Role           string   `json:"role"`
			AllowedActions []string `json:"allowed_actions"`
		} `json:"roles"`
		ExtraCapabilities []struct {
			Action  string   `json:"action"`
			Risk    string   `json:"risk"`
			Implies []string `json:"implies"`
		} `json:"extra_capabilities"`
	}
	decodeInto(t, rec, &page)
	if len(page.Roles) != 2 || len(page.ExtraCapabilities) != 6 {
		t.Fatalf("roles/extras = %d/%d", len(page.Roles), len(page.ExtraCapabilities))
	}
	for _, extra := range page.ExtraCapabilities {
		if extra.Action == authz.BackupsRestore.String() && !strings.Contains(extra.Risk, "delete backup") {
			t.Errorf("backup restore risk does not mention archive deletion: %q", extra.Risk)
		}
	}
}

func TestGrantRoutesRejectMembersAndUnsafeCapabilities(t *testing.T) {
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	path := "/api/v1/instances/inst-a/grants/u-member"

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/grants", http.NoBody),
		httptest.NewRequest(http.MethodGet, path, http.NoBody),
		grantRequest(t, http.MethodPut, path, `"anything"`, map[string]any{"role": "viewer", "perms": []string{}}),
		grantRequest(t, http.MethodDelete, path, `"anything"`, nil),
	} {
		if rec := as(rt, member, req); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as member = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}

	for _, capability := range []string{"users.manage", "not.a.capability"} {
		rec := as(rt, admin, grantRequest(t, http.MethodPut, path, `"anything"`, map[string]any{
			"role": "viewer", "perms": []string{capability},
		}))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("capability %q = %d, want 422 (%s)", capability, rec.Code, rec.Body)
		}
	}
}

func TestGrantChangeIsAnnouncedOnlyAfterCommit(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-other', 'bea', 'argon2id$stub', 'member', ?)`, store.Now())
	changes := &grantChangeRecorder{}
	handler := &Grants{DB: db, Authz: authz.New(db), Changes: changes}
	path := "/api/v1/instances/inst-a/grants/u-other"

	failedRequest := grantRequest(t, http.MethodPut, path, "*", map[string]any{
		"role": "viewer", "perms": []string{},
	})
	failedRequest.SetPathValue("id", "inst-a")
	failedRequest.SetPathValue("user_id", "u-other")
	failed := httptest.NewRecorder()
	handler.put(failed, failedRequest.WithContext(middleware.WithUser(
		failedRequest.Context(), &store.User{ID: "missing-admin", Role: store.RoleAdmin},
	)))
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("failed transaction = %d, want 500 (%s)", failed.Code, failed.Body)
	}
	if len(changes.calls) != 0 {
		t.Fatalf("failed transaction announced changes: %v", changes.calls)
	}
	if grant, err := db.GrantRecordFor(t.Context(), "u-other", "inst-a"); err != nil || grant != nil {
		t.Fatalf("failed transaction stored grant %+v, err %v", grant, err)
	}

	successRequest := grantRequest(t, http.MethodPut, path, "*", map[string]any{
		"role": "viewer", "perms": []string{},
	})
	successRequest.SetPathValue("id", "inst-a")
	successRequest.SetPathValue("user_id", "u-other")
	success := httptest.NewRecorder()
	handler.put(success, successRequest.WithContext(middleware.WithUser(successRequest.Context(), admin)))
	if success.Code != http.StatusCreated {
		t.Fatalf("successful transaction = %d, want 201 (%s)", success.Code, success.Body)
	}
	if len(changes.calls) != 1 || changes.calls[0] != [2]string{"u-other", "inst-a"} {
		t.Fatalf("announcements = %v, want one committed change", changes.calls)
	}
}
