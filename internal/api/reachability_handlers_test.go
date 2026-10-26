package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/reachability"
)

const reachabilityPath = "/api/v1/instances/inst-a/reachability"

// A server that is stopped has nothing to measure, and says so.
func TestReachabilityOfAStoppedServer(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	rt.instances.Reachability = reachability.New(&http.Client{Transport: refuseNetwork{t}}, "https://lookup.invalid")

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got reachability.Report
	decodeInto(t, rec, &got)
	if got.Status != reachability.StatusUnknown || !strings.Contains(got.Detail, "not running") {
		t.Errorf("report = %+v, want unknown because the server is not running", got)
	}
	if got.Steps == nil {
		t.Error("steps is null, want an empty list")
	}
}

// A crossplay server joins by code, so the check reads the instance's crossplay setting and
// its join code and touches no network.
func TestReachabilityOfACrossplayServerWaitsForItsJoinCode(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "running")
	seed(t, db, `UPDATE instances SET crossplay = 1 WHERE id = 'inst-a'`)
	rt.instances.Reachability = reachability.New(&http.Client{Transport: refuseNetwork{t}}, "https://lookup.invalid")

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got reachability.Report
	decodeInto(t, rec, &got)
	if got.Status != reachability.StatusWarn || !strings.Contains(got.Detail, "join code") || len(got.Steps) != 0 {
		t.Errorf("report = %+v, want a warning that no join code is logged yet and no steps", got)
	}
}

// A running server is measured: with the public IP lookup turned off, the check names the
// lookup as the step that could not be done and touches no network.
func TestReachabilityOfARunningServerNeedsAPublicAddress(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "running")
	rt.instances.Reachability = reachability.New(&http.Client{Transport: refuseNetwork{t}}, "")

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got reachability.Report
	decodeInto(t, rec, &got)
	if got.Status != reachability.StatusUnknown || len(got.Steps) != 1 || got.Steps[0].Name != "public_ip" {
		t.Errorf("report = %+v, want unknown with only the public_ip step", got)
	}
}

// The check measures the instance's own base port, and a server with public listing off is
// reported as one that cannot be tested rather than one that does not answer.
func TestReachabilityOfAServerWithPublicListingOff(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "running")
	lookup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	t.Cleanup(lookup.Close)
	rt.instances.Reachability = reachability.New(lookup.Client(), lookup.URL)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got reachability.Report
	decodeInto(t, rec, &got)
	if got.Status != reachability.StatusUnknown || got.PublicAddress != "203.0.113.7:2456" {
		t.Errorf("report = %+v, want unknown with the join address 203.0.113.7:2456", got)
	}
}

// Settings changed on a running server apply at its next start, so the check says to restart
// instead of measuring a server that does not match them.
func TestReachabilityWaitsForAPendingRestart(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "running")
	seed(t, db, `UPDATE instances SET restart_required = 1 WHERE id = 'inst-a'`)
	rt.instances.Reachability = reachability.New(&http.Client{Transport: refuseNetwork{t}}, "https://lookup.invalid")

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	var got reachability.Report
	decodeInto(t, rec, &got)
	if got.Status != reachability.StatusUnknown || !strings.Contains(got.Detail, "waiting for a restart") {
		t.Errorf("report = %+v, want unknown because a restart is pending", got)
	}
}

type refuseNetwork struct{ t *testing.T }

func (n refuseNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Error("the check used the network")
	return nil, http.ErrNotSupported
}

// A viewer may check, since they need the address to join. Someone who cannot see the server
// learns nothing about it, not even that it exists.
func TestReachabilityFollowsWhoCanSeeTheServer(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "running")
	rt.instances.Reachability = reachability.New(&http.Client{Transport: refuseNetwork{t}}, "")

	rec := as(rt, member, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("viewer = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	seed(t, db, `DELETE FROM instance_grants WHERE user_id = 'u-member'`)
	rec = as(rt, member, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("member without a grant = %d, want 404", rec.Code)
	}
	rec = as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/missing/reachability", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance = %d, want 404", rec.Code)
	}
}

// Each check makes outbound requests, so a user's budget is five a minute.
func TestReachabilityIsRateLimitedPerUser(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	rt.instances.Reachability = reachability.New(&http.Client{Transport: refuseNetwork{t}}, "https://lookup.invalid")

	for i := range reachabilityBurst {
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 (%s)", i+1, rec.Code, rec.Body)
		}
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("request over the burst = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After")
	}
	rec = as(rt, member, httptest.NewRequest(http.MethodGet, reachabilityPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("another user's request = %d, want 200: the budget is per user", rec.Code)
	}
}
