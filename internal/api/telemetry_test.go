package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func readLogPage(t *testing.T, rec *httptest.ResponseRecorder) []logLine {
	t.Helper()
	var got Page[logLine]
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return got.Items
}

// TestLogsAndStatsAreInstanceScopedTwice is D2 on the two reads WP-24 needs. An instance the
// caller cannot see is 404 — a 403 there confirms it exists. One they can see but hold no
// console.read or stats.read on is 403, because pretending it does not exist would be a lie
// they can disprove from their own dashboard.
func TestLogsAndStatsAreInstanceScopedTwice(t *testing.T) {
	rt, db, admin, member := world(t)
	// The member's grant on inst-a is `viewer`, which carries console.read and stats.read.
	seed(t, db, `UPDATE instance_grants SET role = 'viewer' WHERE user_id = 'u-member'`)

	for _, path := range []string{"logs", "stats"} {
		for _, tc := range []struct {
			name   string
			user   *store.User
			id     string
			status int
		}{
			{"admin, visible", admin, "inst-a", http.StatusOK},
			{"member, granted", member, "inst-a", http.StatusOK},
			{"member, invisible", member, "inst-b", http.StatusNotFound},
		} {
			t.Run(path+" "+tc.name, func(t *testing.T) {
				rec := as(rt, tc.user, httptest.NewRequest(
					http.MethodGet, "/api/v1/instances/"+tc.id+"/"+path, http.NoBody))
				if rec.Code != tc.status {
					t.Errorf("GET %s = %d, want %d (%s)", path, rec.Code, tc.status, rec.Body)
				}
			})
		}
	}
}

// TestLogsOfANeverProvisionedInstanceIsEmptyNotAnError: the caller asked what the server
// said, and it has not said anything. That is an empty answer, not a failure.
func TestLogsOfANeverProvisionedInstanceIsEmptyNotAnError(t *testing.T) {
	rt, _, admin, _ := world(t)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/logs", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if lines := readLogPage(t, rec); len(lines) != 0 {
		t.Errorf("lines = %+v, want none", lines)
	}
}

func TestLogTailIsClampedRatherThanRejected(t *testing.T) {
	rt, _, admin, _ := world(t)

	// 11 §4's rule for limits, applied to this one: a client asking for more than the panel
	// will serve gets the maximum, not an error it has to special-case.
	for _, tail := range []string{"1", "999999", "0", "-5"} {
		rec := as(rt, admin, httptest.NewRequest(
			http.MethodGet, "/api/v1/instances/inst-a/logs?tail="+tail, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Errorf("tail=%s = %d, want 200", tail, rec.Code)
		}
	}
	// A value that is not a number at all is a malformed request, and says which parameter.
	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/logs?tail=lots", http.NoBody))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("tail=lots = %d, want 400", rec.Code)
	}
}

// TestStatsOfAStoppedInstanceReportsUnavailableRatherThanZeros is E10's lesson applied to
// the one-shot read: a stopped server has no resource usage, and zeros would be a confident
// number for something the panel does not know.
func TestStatsOfAStoppedInstanceReportsUnavailableRatherThanZeros(t *testing.T) {
	rt, _, admin, _ := world(t)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/stats", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var got statsView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Error("a stopped instance reported stats as available")
	}
	if got.CPUPct != nil || got.MemBytes != nil {
		t.Errorf("a stopped instance reported numbers: %+v", got)
	}
	// And the field says null rather than being absent, on this route as on the socket (E7).
	if !strings.Contains(rec.Body.String(), `"players":null`) {
		t.Errorf("players is not reported as null: %s", rec.Body)
	}
}

// TestJobHistoryIsInstanceScopedAndPaged covers ADR-099's route: D2's 404 for an instance
// this caller cannot see, and the keyset page 11 §4 requires.
//
// `↯` The scoping matters more here than on most reads. A job row carries `instance_name`
// denormalised so history stays readable after a delete (C15), so a list that leaked past
// its instance would hand a member the names of servers they cannot otherwise see.
func TestJobHistoryIsInstanceScopedAndPaged(t *testing.T) {
	rt, db, admin, member := world(t)

	base := time.Now()
	for i, id := range []string{"j-1", "j-2", "j-3"} {
		seed(t, db, `
			INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at)
			VALUES (?, 'start', 'succeeded', ?, 'inst-a', 'a', '{}', ?)`,
			id, "lock-"+id, store.FormatTime(base.Add(time.Duration(i)*time.Second)))
	}
	seed(t, db, `
		INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at)
		VALUES ('j-b', 'start', 'succeeded', 'lock-j-b', 'inst-b', 'b', '{}', ?)`, store.FormatTime(base))

	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-b/jobs", http.NoBody)); rec.Code != http.StatusNotFound {
		t.Errorf("member on an invisible instance = %d, want 404 (%s)", rec.Code, rec.Body)
	}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/jobs?limit=2", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var first Page[jobView]
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(first.Items) != 2 || first.Items[0].JobID != "j-3" {
		t.Fatalf("first page = %+v, want j-3 then j-2", first.Items)
	}
	if first.NextCursor == nil {
		t.Fatal("a page that left a row behind must carry a next_cursor")
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-a/jobs?limit=2&cursor="+*first.NextCursor, http.NoBody))
	var second Page[jobView]
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(second.Items) != 1 || second.Items[0].JobID != "j-1" {
		t.Fatalf("second page = %+v, want j-1 alone", second.Items)
	}
	if second.NextCursor != nil {
		t.Error("the last page must end with next_cursor null, not an empty page after it")
	}
	for _, j := range append(first.Items, second.Items...) {
		if j.InstanceID == nil || *j.InstanceID != "inst-a" {
			t.Errorf("job %s belongs to another instance", j.JobID)
		}
	}
}

// TestDiskIsProtectedLikeEveryOtherInstanceRead is D1 and D2 on the new route: an instance
// the caller cannot see is 404, never 403, because a 403 confirms it exists (ADR-038); one
// they can see but hold no stats.read on is 403, because pretending it does not exist would
// be a lie they can disprove from their own dashboard.
func TestDiskIsProtectedLikeEveryOtherInstanceRead(t *testing.T) {
	rt, db, admin, member := world(t)
	seed(t, db, `UPDATE instance_grants SET role = 'viewer' WHERE user_id = 'u-member'`)

	for _, tc := range []struct {
		name   string
		user   *store.User
		id     string
		status int
	}{
		{"admin, visible", admin, "inst-a", http.StatusOK},
		{"member, granted viewer", member, "inst-a", http.StatusOK},
		{"member, invisible", member, "inst-b", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := as(rt, tc.user, httptest.NewRequest(
				http.MethodGet, "/api/v1/instances/"+tc.id+"/disk", http.NoBody))
			if rec.Code != tc.status {
				t.Errorf("GET disk = %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
		})
	}

	// `↯` The handler's second check — 403 for a caller who can see the instance but holds
	// no stats.read — is **not reachable through the grant model today**, and that is worth
	// writing down rather than faking with a hand-built grant. 09 §3.1 puts stats.read in the
	// viewer base set and Can only ever *unions* a grant's perms onto its role, so there is
	// no way to hold instance.view without it: the roles are exactly viewer and operator, a
	// CHECK constraint enforces that, and both include it.
	//
	// The check is still correct and still required (ADR-037: every handler authorizes the
	// action it performs, at its own call site). It goes live the day 09 §3 grows a narrower
	// role, which is precisely the retrofit ADR-021 says must never be needed. The same is
	// true of /logs and /stats.
}

// An instance that was never provisioned has no server/ and no backups. That measures as
// zero rather than failing: the caller asked how much space it uses, and the answer is none.
func TestDiskOfANeverProvisionedInstanceIsZeroNotAnError(t *testing.T) {
	rt, _, admin, _ := world(t)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/disk", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got diskView
	decodeInto(t, rec, &got)
	if got.TotalBytes != 0 {
		t.Errorf("total = %d, want 0", got.TotalBytes)
	}
	if got.MeasuredAt.IsZero() {
		t.Error("a reading must say when it was taken — it is a walk, not a live sample")
	}
}
