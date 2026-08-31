package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
