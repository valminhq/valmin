package reachability

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func steamServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ISteamApps/GetServersAtAddress/v1/" || r.URL.Query().Get("addr") != "203.0.113.7" {
			t.Errorf("request = %s, want the listing for 203.0.113.7", r.URL)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRegistered(t *testing.T) {
	fixture, err := os.ReadFile("testdata/servers-at-address.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	const noServers = `{"response":{"success":true,"servers":[]}}`
	tests := []struct {
		name     string
		status   int
		body     string
		basePort int
		want     bool
		wantErr  bool
	}{
		{name: "listed on the game port", status: 200, body: string(fixture), basePort: 2456, want: true},
		{name: "another server on the host", status: 200, body: string(fixture), basePort: 2461},
		{name: "nothing listed", status: 200, body: noServers, basePort: 2456},
		{
			name: "another game on the port", status: 200, basePort: 2456,
			body: strings.Replace(string(fixture), `"appid":892970`, `"appid":730`, 1),
		},
		{
			name: "only the query port in the address", status: 200, basePort: 2456,
			body: `{"response":{"success":true,"servers":[{"addr":"203.0.113.7:2457","appid":892970}]}}`,
			want: true,
		},
		{
			name: "only the game port field", status: 200, basePort: 2456,
			body: `{"response":{"success":true,"servers":[{"addr":"203.0.113.7:9","appid":892970,"gameport":2456}]}}`,
			want: true,
		},
		{name: "steam reports failure", status: 200, body: `{"response":{"success":false}}`, wantErr: true},
		{name: "steam is down", status: 503, body: "", wantErr: true},
		{name: "not json", status: 200, body: "<html>", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := steamServer(t, tc.status, tc.body)
			c := &Checker{HTTP: srv.Client(), SteamURL: srv.URL}
			got, err := c.registered(t.Context(), netip.MustParseAddr("203.0.113.7"), tc.basePort)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("registered(base %d) = %t, %v; want %t, error: %t", tc.basePort, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestSteamListingMapsErrorsToFailed(t *testing.T) {
	srv := steamServer(t, http.StatusServiceUnavailable, "")
	c := &Checker{HTTP: srv.Client(), SteamURL: srv.URL}
	if got := c.steamListing(t.Context(), netip.MustParseAddr("203.0.113.7"), 2456); got != listingFailed {
		t.Errorf("steamListing() = %v, want listingFailed", got)
	}
}
