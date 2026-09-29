package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestViewerCannotCancelAJobRequiringMorePermission asserts 12 §8: cancellation requires the
// action that would have started the job, not merely instance.view. mel holds viewer only on
// inst-a.
func TestViewerCannotCancelAJobRequiringMorePermission(t *testing.T) {
	rt, db, _, mel := world(t)
	seedJob(t, db, "job-start", "start", "running", ptr("inst-a"))

	rec := as(rt, mel, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-start/cancel", http.NoBody))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer cancel start job = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "forbidden" {
		t.Errorf("code = %q, want forbidden", got)
	}
}

// TestCancelWithMatchingPermissionReachesTheEngine proves the authorization check, once
// satisfied, does not itself block cancellation: a user with instance.start still hits the
// engine's own cancellability rule (job_not_cancellable), not a permission denial.
func TestCancelWithMatchingPermissionReachesTheEngine(t *testing.T) {
	rt, db, _, _ := world(t)
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-op', 'oli', 'argon2id$stub', 'member', ?)`, store.Now())
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-op', 'inst-a', 'operator', '[]', ?)`, store.Now())
	operator := &store.User{ID: "u-op", Username: "oli", Role: store.RoleMember}
	seedJob(t, db, "job-start", "start", "running", ptr("inst-a"))

	rec := as(rt, operator, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-start/cancel", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("operator cancel start job = %d, want 409 job_not_cancellable (%s)", rec.Code, rec.Body)
	}
}

// TestCancelUnmappedKindIsDeniedEvenForAdmin is 12 §8's fail-closed default: a kind with no
// row in cancelActions cannot be cancelled by anyone, admin included, until one is added.
func TestCancelUnmappedKindIsDeniedEvenForAdmin(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedJob(t, db, "job-bogus", "bogus_kind", "running", ptr("inst-a"))

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-bogus/cancel", http.NoBody))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin cancel unmapped kind = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

func ptr(s string) *string { return &s }

// TestCancelIsAuditedOnlyWhenAccepted asserts that a cancel the engine took writes one entry
// naming the job and its kind, and a cancel it refused writes none.
func TestCancelIsAuditedOnlyWhenAccepted(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedJob(t, db, "job-refused", "start", "running", ptr("inst-a"))
	seedJob(t, db, "job-queued", "start", "queued", ptr("inst-a"))

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-refused/cancel", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel of a running start job = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := lifecycleAuditRows(t, db, "jobs.cancel"); len(got) != 0 {
		t.Fatalf("audit rows after a refused cancel = %+v, want none", got)
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-queued/cancel", http.NoBody))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel of a queued job = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	want := lifecycleAuditRow{
		UserID: admin.ID, ActorName: admin.Username, InstanceID: "inst-a", InstanceName: "inst-a",
		Action: "jobs.cancel", Detail: `{"job_id":"job-queued","kind":"start"}`, IP: "192.0.2.1",
		Outcome: store.AuditSucceeded,
	}
	if got := lifecycleAuditRows(t, db, want.Action); len(got) != 1 || got[0] != want {
		t.Errorf("audit rows after an accepted cancel = %+v, want exactly %+v", got, want)
	}
}

// Asserts each mod job kind reads its changes from the payload, and that any other kind or an
// unreadable payload reads none.
func TestJobChangesReadThePayload(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload string
		want                []jobChange
	}{
		{
			"single install", "mod_install", `{"full_name":"Foo-Bar","version":"1.3.0"}`,
			[]jobChange{{Action: "install", FullName: "Foo-Bar", ToVersion: "1.3.0"}},
		},
		{
			"install at a minimum names no version", "mod_install",
			`{"full_name":"Foo-Bar","version":"1.3.0","minimum":true}`,
			[]jobChange{{Action: "install", FullName: "Foo-Bar"}},
		},
		{
			"update all", "mod_install",
			`{"updates":[{"full_name":"Foo-Bar","from_version":"1.2.0","version":"1.3.0"},{"full_name":"Baz-Qux","version":"2.0.0"}]}`,
			[]jobChange{
				{Action: "update", FullName: "Foo-Bar", FromVersion: "1.2.0", ToVersion: "1.3.0"},
				{Action: "update", FullName: "Baz-Qux", ToVersion: "2.0.0"},
			},
		},
		{"install with no package", "mod_install", `{}`, nil},
		{
			"uninstall", "mod_uninstall", `{"full_names":["Foo-Bar","Baz-Qux"]}`,
			[]jobChange{{Action: "uninstall", FullName: "Foo-Bar"}, {Action: "uninstall", FullName: "Baz-Qux"}},
		},
		{"uninstall of nothing", "mod_uninstall", `{}`, nil},
		{
			"toggle off", "mod_toggle", `{"full_name":"Foo-Bar","enable":false}`,
			[]jobChange{{Action: "disable", FullName: "Foo-Bar"}},
		},
		{
			"toggle on", "mod_toggle", `{"full_name":"Foo-Bar","enable":true}`,
			[]jobChange{{Action: "enable", FullName: "Foo-Bar"}},
		},
		{"unreadable payload", "mod_install", `not json`, nil},
		{"other kind", "backup", `{"full_name":"Foo-Bar","version":"1.3.0"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := jobChanges(&store.Job{Kind: tc.kind, Payload: tc.payload})
			if !slices.Equal(got, tc.want) {
				t.Errorf("jobChanges(%s %s) = %+v, want %+v", tc.kind, tc.payload, got, tc.want)
			}
		})
	}
}

// Asserts the job history and GET /jobs/{id} name the requester and mark scheduled runs, and
// leave the name out for system work and for a requester who no longer exists.
func TestJobViewsNameTheirRequester(t *testing.T) {
	rt, db, admin, member := world(t)
	seed(
		t,
		db,
		`INSERT INTO scheduled_jobs (id, instance_id, kind, cron) VALUES ('s-1', 'inst-a', 'backup', '0 3 * * *')`,
	)
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-gone', 'gil', 'argon2id$stub', 'member', ?)`, store.Now())
	for _, row := range []struct {
		id          string
		requestedBy any
		scheduleID  any
	}{
		{"j-user", "u-admin", nil},
		{"j-sched", nil, "s-1"},
		{"j-system", nil, nil},
		{"j-gone", "u-gone", nil},
	} {
		seed(t, db, `
			INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, schedule_id, scheduled, requested_by, payload, created_at)
			VALUES (?, 'backup', 'succeeded', ?, 'inst-a', 'a', ?, ?, ?, '{}', ?)`,
			row.id, "lock-"+row.id, row.scheduleID, row.scheduleID != nil, row.requestedBy, store.Now())
	}
	seed(t, db, `DELETE FROM users WHERE id = 'u-gone'`)

	for _, caller := range []*store.User{admin, member} {
		rec := as(rt, caller, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/jobs", http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("history as %s = %d, want 200 (%s)", caller.Username, rec.Code, rec.Body)
		}
		var page Page[jobView]
		decodeInto(t, rec, &page)
		byID := map[string]jobView{}
		for _, j := range page.Items {
			byID[j.JobID] = j
		}
		for id, want := range map[string]string{"j-user": "ada", "j-sched": "", "j-system": "", "j-gone": ""} {
			got := ""
			if name := byID[id].RequestedByName; name != nil {
				got = *name
			}
			if got != want {
				t.Errorf("%s: %s requested_by_name = %q, want %q", caller.Username, id, got, want)
			}
		}
		if !byID["j-sched"].Scheduled || byID["j-user"].Scheduled {
			t.Errorf("%s: scheduled flags = %v/%v, want true for j-sched only", caller.Username,
				byID["j-sched"].Scheduled, byID["j-user"].Scheduled)
		}
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/j-user", http.NoBody))
	var one jobView
	decodeInto(t, rec, &one)
	if one.RequestedByName == nil || *one.RequestedByName != "ada" {
		t.Errorf("GET /jobs/j-user requested_by_name = %v, want ada", one.RequestedByName)
	}
}
