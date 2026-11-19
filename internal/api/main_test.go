package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/config"
)

// TestMain runs the package's tests with the network out of reach: no name resolves through DNS
// and no proxy is used, so a call that would leave the machine fails at once, whatever HTTP
// client makes it. A test then passes or fails the same on every machine, instead of waiting on
// the internet and tripping a route's timeout on a slow day. Test servers listen on loopback
// addresses and need neither. The proxy variables are cleared before any client reads them,
// since Go reads them once.
func TestMain(m *testing.M) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		_ = os.Unsetenv(name)
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("tests do not reach the network")
		},
	}
	os.Exit(m.Run())
}

// testConfig is the default configuration with both registries served by a local fake, so the
// diagnostics probe, the catalogue sync and a Thunderstore profile upload answer at once and
// reach no one. Hexium's profile host is fixed in the code; a test that uses it sets Profiles.
func testConfig(t testing.TB) config.Config {
	t.Helper()
	cfg := config.Defaults()
	url := fakeRegistry(t)
	cfg.Thunderstore.BaseURL = url
	cfg.Hexium.BaseURL = url
	return cfg
}

// fakeRegistry serves an empty package listing, accepts every profile upload, and knows no
// profile code, as a registry with nothing stored would.
func fakeRegistry(t testing.TB) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"key":"`+profileKey+`"}`)
		case strings.Contains(r.URL.Path, "/get/"):
			http.NotFound(w, r)
		default:
			_, _ = io.WriteString(w, "[]")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
