package reachability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

var publicIP = netip.MustParseAddr("203.0.113.7")

func TestVerdict(t *testing.T) {
	running := Target{BasePort: 2456, Running: true}
	public := Target{BasePort: 2456, Running: true, Public: true}
	tests := []struct {
		name        string
		facts       facts
		want        Status
		wantAddress string
		wantDetail  string
		wantRemedy  bool
	}{
		{
			name:       "stopped",
			facts:      facts{Target: Target{BasePort: 2456}},
			want:       StatusUnknown,
			wantDetail: "not running",
			wantRemedy: true,
		},
		{
			name:       "settings waiting for a restart",
			facts:      facts{Target: Target{BasePort: 2456, Running: true, Public: true, RestartPending: true}},
			want:       StatusUnknown,
			wantDetail: "waiting for a restart",
			wantRemedy: true,
		},
		{
			name:       "crossplay without a code",
			facts:      facts{Target: Target{BasePort: 2456, Running: true, Crossplay: true}},
			want:       StatusWarn,
			wantDetail: "has not logged a join code",
			wantRemedy: true,
		},
		{
			name:       "crossplay with a code",
			facts:      facts{Target: Target{BasePort: 2456, Running: true, Crossplay: true, JoinCode: "111111"}},
			want:       StatusOK,
			wantDetail: "join code",
		},
		{
			name:       "public address unknown",
			facts:      facts{Target: running},
			want:       StatusUnknown,
			wantDetail: "could not be determined",
			wantRemedy: true,
		},
		{
			name:        "not public, so not tested",
			facts:       facts{Target: running, ip: publicIP},
			want:        StatusUnknown,
			wantAddress: "203.0.113.7:2456",
			wantDetail:  "only while public listing is on",
			wantRemedy:  true,
		},
		{
			name:        "answers and is listed",
			facts:       facts{Target: public, ip: publicIP, answered: true, listing: listingFound},
			want:        StatusOK,
			wantAddress: "203.0.113.7:2456",
			wantDetail:  "answers at 203.0.113.7:2456",
		},
		{
			name:        "answers but steam does not list it",
			facts:       facts{Target: public, ip: publicIP, answered: true, listing: listingMissing},
			want:        StatusWarn,
			wantAddress: "203.0.113.7:2456",
			wantDetail:  "Steam does not list it",
			wantRemedy:  true,
		},
		{
			name:        "answers and steam could not be asked",
			facts:       facts{Target: public, ip: publicIP, answered: true, listing: listingFailed},
			want:        StatusOK,
			wantAddress: "203.0.113.7:2456",
			wantDetail:  "answers at",
		},
		{
			name:        "no answer although steam lists it",
			facts:       facts{Target: public, ip: publicIP, listing: listingFound},
			want:        StatusWarn,
			wantAddress: "203.0.113.7:2456",
			wantDetail:  "did not answer at 203.0.113.7:2456",
			wantRemedy:  true,
		},
		{
			name:        "no answer",
			facts:       facts{Target: public, ip: publicIP, listing: listingMissing},
			want:        StatusWarn,
			wantAddress: "203.0.113.7:2456",
			wantDetail:  "did not answer at 203.0.113.7:2456",
			wantRemedy:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verdict(tc.facts)
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s", got.Status, tc.want)
			}
			if got.PublicAddress != tc.wantAddress {
				t.Errorf("public address = %q, want %q", got.PublicAddress, tc.wantAddress)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tc.wantDetail)
			}
			if (got.Remedy != "") != tc.wantRemedy {
				t.Errorf("remedy = %q, want a remedy: %t", got.Remedy, tc.wantRemedy)
			}
		})
	}
}

func TestPublicIPv4(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"203.0.113.7", true},
		{"8.8.8.8", true},
		{"192.168.1.5", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"127.0.0.1", false},
		{"169.254.1.1", false},
		{"100.64.1.1", false},
		{"100.101.102.103", false},
		{"0.1.2.3", false},
		{"240.0.0.1", false},
		{"198.18.0.1", false},
		{"192.0.0.8", false},
		{"224.0.0.1", false},
		{"::1", false},
		{"2001:db8::1", false},
	}
	for _, tc := range tests {
		if got := publicIPv4(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("publicIPv4(%s) = %t, want %t", tc.ip, got, tc.want)
		}
	}
}

func TestUnreachableRemedyNamesTheCausesAndPorts(t *testing.T) {
	got := unreachableRemedy(publicIP, 2461)
	for _, want := range []string{"2461-2462", "firewall", "NAT loopback", "203.0.113.7:2461", "100.64.0.0/10", "crossplay"} {
		if !strings.Contains(got, want) {
			t.Errorf("remedy = %q, want it to mention %q", got, want)
		}
	}
}

func lookupServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stepStatus(r *Report, name string) (Status, bool) {
	for _, s := range r.Steps {
		if s.Name == name {
			return s.Status, true
		}
	}
	return "", false
}

func TestCheckWithoutAPublicAddress(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		disabled   bool
		wantStep   Status
		wantDetail string
	}{
		{name: "lookup turned off", disabled: true, wantStep: StatusUnknown, wantDetail: "turned off"},
		{name: "service error", status: 500, wantStep: StatusWarn},
		{name: "not an address", status: 200, body: "<html>", wantStep: StatusWarn},
		{name: "a private address", status: 200, body: "192.168.1.5", wantStep: StatusWarn},
		{name: "an IPv6 address", status: 200, body: "2001:db8::1", wantStep: StatusWarn},
		{name: "a shared address", status: 200, body: "100.101.102.103", wantStep: StatusWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := lookupServer(t, tc.status, tc.body)
			c := New(srv.Client(), srv.URL)
			if tc.disabled {
				c.PublicIPURL = ""
			}
			r := c.Check(t.Context(), Target{BasePort: 2456, Running: true})
			if r.Status != StatusUnknown {
				t.Errorf("status = %s, want unknown", r.Status)
			}
			if got, ok := stepStatus(&r, "public_ip"); !ok || got != tc.wantStep {
				t.Errorf("public_ip step = %q, %t; want %q", got, ok, tc.wantStep)
			}
			if len(r.Steps) != 1 {
				t.Fatalf("steps = %+v, want only the lookup", r.Steps)
			}
			if !strings.Contains(r.Steps[0].Detail, tc.wantDetail) {
				t.Errorf("step detail = %q, want it to contain %q", r.Steps[0].Detail, tc.wantDetail)
			}
			if r.CheckedAt.IsZero() {
				t.Error("CheckedAt is zero")
			}
		})
	}
}

// queryTo returns a query that records the address it was sent to and answers with err.
func queryTo(sent *string, err error) func(context.Context, string) error {
	return func(_ context.Context, hostPort string) error {
		*sent = hostPort
		return err
	}
}

// A public server is queried on its query port, base+1, and looked up at Steam.
func TestCheckOfAPublicServer(t *testing.T) {
	const noServers = `{"response":{"success":true,"servers":[]}}`
	tests := []struct {
		name       string
		steamBody  string
		queryErr   error
		want       Status
		wantListed Status
		wantQuery  Status
	}{
		{
			name: "answers and is listed", steamBody: steamFixture(t), queryErr: nil,
			want: StatusOK, wantListed: StatusOK, wantQuery: StatusOK,
		},
		{
			name:       "listed by its game port alone",
			steamBody:  `{"response":{"success":true,"servers":[{"addr":"203.0.113.7:9","appid":892970,"gameport":2456}]}}`,
			want:       StatusOK,
			wantListed: StatusOK,
			wantQuery:  StatusOK,
		},
		{
			name:       "only a neighbouring server is listed",
			steamBody:  `{"response":{"success":true,"servers":[{"addr":"203.0.113.7:2462","appid":892970,"gameport":2461}]}}`,
			want:       StatusWarn,
			wantListed: StatusWarn,
			wantQuery:  StatusOK,
		},
		{
			name: "answers but is not listed", steamBody: noServers, queryErr: nil,
			want: StatusWarn, wantListed: StatusWarn, wantQuery: StatusOK,
		},
		{
			name: "listed but does not answer", steamBody: steamFixture(t), queryErr: errors.New("no reply"),
			want: StatusWarn, wantListed: StatusOK, wantQuery: StatusWarn,
		},
		{
			name: "neither", steamBody: noServers, queryErr: errors.New("no reply"),
			want: StatusWarn, wantListed: StatusWarn, wantQuery: StatusWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			steam := steamServer(t, http.StatusOK, tc.steamBody)
			ip := lookupServer(t, http.StatusOK, "203.0.113.7\n")
			c := New(steam.Client(), ip.URL)
			c.SteamURL = steam.URL
			var sent string
			c.query = queryTo(&sent, tc.queryErr)

			r := c.Check(t.Context(), Target{BasePort: 2456, Running: true, Public: true})

			if sent != "203.0.113.7:2457" {
				t.Errorf("queried %q, want the query port 203.0.113.7:2457", sent)
			}
			if r.Status != tc.want || r.PublicAddress != "203.0.113.7:2456" {
				t.Errorf("report = %+v, want %s with the join address 203.0.113.7:2456", r, tc.want)
			}
			for name, want := range map[string]Status{
				"public_ip": StatusOK, "steam_listing": tc.wantListed, "server_query": tc.wantQuery,
			} {
				if got, ok := stepStatus(&r, name); !ok || got != want {
					t.Errorf("%s step = %q, %t; want %q", name, got, ok, want)
				}
			}
		})
	}
}

func steamFixture(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/servers-at-address.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(body)
}

// A server answers Steam queries only while public listing is on, so another server is neither
// queried nor looked up at Steam, and the report says it could not be tested.
func TestCheckDoesNotQueryAServerThatIsNotPublic(t *testing.T) {
	steam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Steam was asked for %s", r.URL)
	}))
	t.Cleanup(steam.Close)
	ip := lookupServer(t, http.StatusOK, "203.0.113.7")
	c := New(steam.Client(), ip.URL)
	c.SteamURL = steam.URL
	c.query = func(_ context.Context, hostPort string) error {
		t.Errorf("a server with public listing off was queried at %s", hostPort)
		return nil
	}

	r := c.Check(t.Context(), Target{BasePort: 2456, Running: true})
	if r.Status != StatusUnknown || r.PublicAddress != "203.0.113.7:2456" {
		t.Errorf("report = %+v, want unknown with the join address", r)
	}
	if got, ok := stepStatus(&r, "server_query"); !ok || got != StatusUnknown {
		t.Errorf("server_query step = %q, %t; want unknown", got, ok)
	}
	if _, ok := stepStatus(&r, "steam_listing"); ok {
		t.Errorf("steps = %+v, want no Steam step", r.Steps)
	}
}

// A settings change waiting for a restart is reported before anything is measured.
func TestCheckSkipsTheNetworkWhileARestartIsPending(t *testing.T) {
	c := New(&http.Client{Transport: noNetwork{t}}, "http://lookup.invalid")
	r := c.Check(t.Context(), Target{BasePort: 2456, Running: true, Public: true, RestartPending: true})
	if r.Status != StatusUnknown || len(r.Steps) != 0 {
		t.Errorf("report = %+v, want unknown with no steps", r)
	}
}

type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Error("the check used the network")
	return nil, http.ErrNotSupported
}

// A server that is stopped, or that joins by code, has nothing to measure from outside.
func TestCheckSkipsTheNetwork(t *testing.T) {
	c := New(&http.Client{Transport: noNetwork{t}}, "http://lookup.invalid")
	for _, target := range []Target{
		{BasePort: 2456},
		{BasePort: 2456, Running: true, Crossplay: true, JoinCode: "111111"},
	} {
		r := c.Check(t.Context(), target)
		if len(r.Steps) != 0 {
			t.Errorf("Check(%+v) steps = %+v, want none", target, r.Steps)
		}
	}
}
