package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// provisionWorld is world()'s shape, but with a real, writable Data.Root/HostRoot: the
// provision job's Runner touches the filesystem the moment Submit returns (it runs in its
// own goroutine), and world()'s router points Data.Root at /srv/valmin, which does not
// exist in a test environment.
func provisionWorld(t *testing.T) (rt *Router, db *store.DB, admin, member *store.User) {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Data.Root = dir
	cfg.Data.HostRoot = dir

	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := health(t)

	rt, err = NewRouter(&cfg, h.DB, h, k, false, testEngine(h.DB, &cfg), runtime.NewFake())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	for _, u := range []struct {
		id, name string
		role     store.Role
	}{{"u-admin", "ada", store.RoleAdmin}, {"u-member", "mel", store.RoleMember}} {
		seed(t, h.DB, `INSERT INTO users (id, username, password_hash, role, created_at)
			VALUES (?, ?, 'argon2id$stub', ?, ?)`, u.id, u.name, string(u.role), store.Now())
	}

	return rt, h.DB,
		&store.User{ID: "u-admin", Username: "ada", Role: store.RoleAdmin},
		&store.User{ID: "u-member", Username: "mel", Role: store.RoleMember}
}

func validCreateBody(name string) map[string]any {
	return map[string]any{
		"name": name, "server_name": "My Server", "world_name": "MyWorld", "password": "hunter2",
	}
}

// TestCreateInstanceRequiresAdmin is 09 §3.3: instance.create is never grantable, so a
// member is rejected before any validation runs.
func TestCreateInstanceRequiresAdmin(t *testing.T) {
	rt, _, _, member := provisionWorld(t)
	rec := as(rt, member, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("member-cannot"))))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestCreateInstanceValidatesLaunchConfig is 03 §1.3's three rules, checked at this
// boundary in addition to instance.BuildSpec's own re-check (G2).
func TestCreateInstanceValidatesLaunchConfig(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	body := map[string]any{
		"name": "bad-config", "server_name": "same", "world_name": "same", "password": "abc",
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}

	var got struct {
		Error struct {
			Details struct {
				Fields []struct {
					Field string `json:"field"`
					Code  string `json:"code"`
				} `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &got)
	if len(got.Error.Details.Fields) < 2 {
		t.Errorf("fields = %+v, want at least password_too_short and world_same_as_server",
			got.Error.Details.Fields)
	}
}

// TestCreateInstanceRequiresAName is the one field 03 §1.3 does not already cover: the
// panel's own unique label, distinct from the in-game server_name.
func TestCreateInstanceRequiresAName(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	body := validCreateBody("")
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// TestCreateInstanceRejectsDuplicateName proves store.ErrInstanceNameTaken reaches the
// wire as 409 name_taken, distinct from a port collision.
func TestCreateInstanceRejectsDuplicateName(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)

	first := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("dupe-name"))))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create status = %d, want 202 (%s)", first.Code, first.Body)
	}

	second := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("dupe-name"))))
	if second.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409 (%s)", second.Code, second.Body)
	}
	if got := errCode(t, second); got != "name_taken" {
		t.Errorf("code = %q, want name_taken", got)
	}
}

// TestCreateInstanceReturns202AndMovesInstanceToProvisioning is 11 §3's contract end to
// end: the response is a job, never the instance, and by the time it is written the
// instance row already exists and has already been claimed into `provisioning` — both
// happen synchronously before Submit returns (12 §6's Claim phase).
func TestCreateInstanceReturns202AndMovesInstanceToProvisioning(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("fresh-instance"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/api/v1/jobs/") {
		t.Errorf("Location = %q, want /api/v1/jobs/...", loc)
	}

	var stub struct {
		JobID  string `json:"job_id"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	decodeInto(t, rec, &stub)
	if stub.JobID == "" {
		t.Error("job_id is empty")
	}
	if stub.Kind != "provision" {
		t.Errorf("kind = %q, want provision", stub.Kind)
	}
	if stub.Status != "running" {
		t.Errorf("status = %q, want running (claim already committed, 11 §3)", stub.Status)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE name = ?`, "fresh-instance").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "provisioning" {
		t.Errorf("instance state = %q, want provisioning", state)
	}
}
