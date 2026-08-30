package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// seedJob inserts a job_runs row directly, mirroring seed()'s pattern elsewhere in this
// package — these tests are about the HTTP layer's authorization and error shape, not the
// engine's own claim/finish mechanics, which internal/jobs already covers.
func seedJob(t *testing.T, db *store.DB, id, kind, status string, instanceID *string) {
	t.Helper()
	seed(t, db, `
		INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at)
		VALUES (?, ?, ?, ?, ?, 'test', '{}', ?)`,
		id, kind, status, "instance:"+id, instanceID, store.Now())
}

// TestJobVisibilityIsInstanceScoped is 09 §4.1's job-topic rule applied to the REST reads:
// resolve the job's instance_id, then require instance.view. A member with a grant on A
// only sees A's job; B's is not_found, not forbidden (D2, ADR-038).
func TestJobVisibilityIsInstanceScoped(t *testing.T) {
	rt, db, admin, member := world(t)
	seedJob(t, db, "job-a", "start", "running", ptr("inst-a"))
	seedJob(t, db, "job-b", "start", "running", ptr("inst-b"))

	for _, tc := range []struct {
		name   string
		user   *store.User
		jobID  string
		status int
	}{
		{"admin sees A", admin, "job-a", http.StatusOK},
		{"admin sees B", admin, "job-b", http.StatusOK},
		{"member sees granted A", member, "job-a", http.StatusOK},
		{"member cannot see ungranted B", member, "job-b", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := as(rt, tc.user, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+tc.jobID, http.NoBody))
			if rec.Code != tc.status {
				t.Fatalf(
					"GET /jobs/%s as %s = %d, want %d (%s)",
					tc.jobID,
					tc.user.Username,
					rec.Code,
					tc.status,
					rec.Body,
				)
			}
		})
	}
}

// TestGlobalJobIsAdminOnly covers 09 §4.1's other rule: a job with no instance_id resolves
// to Can(InstanceView, "") — which a member can never satisfy — so it is admin-only
// without a special case in the handler.
func TestGlobalJobIsAdminOnly(t *testing.T) {
	rt, db, admin, member := world(t)
	seedJob(t, db, "job-global", "thunderstore_sync", "running", nil)

	if rec := as(
		rt,
		admin,
		httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-global", http.NoBody),
	); rec.Code != http.StatusOK {
		t.Fatalf("admin GET global job = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := as(
		rt,
		member,
		httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-global", http.NoBody),
	); rec.Code != http.StatusNotFound {
		t.Fatalf("member GET global job = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestGetUnknownJobIsNotFound proves the missing-row and the invisible-row paths share the
// same envelope, per canSeeJob's design.
func TestGetUnknownJobIsNotFound(t *testing.T) {
	rt, _, admin, _ := world(t)
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/no-such-job", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown job = %d, want 404", rec.Code)
	}
}

// TestCancelWithNoRegisteredPolicyIsNotCancellable covers 12 §8's default for the
// seconds-long kinds (start/stop/restart/delete): with no CancelPolicy registered, a
// running job is never cancellable.
func TestCancelWithNoRegisteredPolicyIsNotCancellable(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedJob(t, db, "job-start", "start", "running", ptr("inst-a"))

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-start/cancel", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel start job = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	var body map[string]any
	decodeInto(t, rec, &body)
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "job_not_cancellable" {
		t.Errorf("code = %v, want job_not_cancellable", errBody["code"])
	}
}

// TestCancelTerminalJobIsNotCancellable covers the terminal row of 12 §8's table.
func TestCancelTerminalJobIsNotCancellable(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedJob(t, db, "job-done", "start", "succeeded", ptr("inst-a"))

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-done/cancel", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel terminal job = %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

// TestCancelUnauthorizedJobIsNotFound proves cancel shares the same visibility rule as get.
func TestCancelUnauthorizedJobIsNotFound(t *testing.T) {
	rt, db, _, member := world(t)
	seedJob(t, db, "job-b", "start", "running", ptr("inst-b"))

	rec := as(rt, member, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-b/cancel", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member cancel B's job = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

func ptr(s string) *string { return &s }
