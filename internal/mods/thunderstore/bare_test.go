package thunderstore

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestListingPaths pins both hosts' listing URLs as literals. The missing community segment
// is the one hard incompatibility between them (03 §6.1), so it is asserted as a string
// rather than only through a server's behaviour.
func TestListingPaths(t *testing.T) {
	tests := []struct {
		name   string
		client *Client
		want   string
	}{
		{"community", New("https://thunderstore.io"), "https://thunderstore.io/c/valheim/api/v1/package/"},
		{"bare", NewBare("https://valheim.hexium.gg"), "https://valheim.hexium.gg/api/v1/package/"},
		{"trailing slash trimmed", NewBare("https://valheim.hexium.gg/"), "https://valheim.hexium.gg/api/v1/package/"},
		{
			"struct literal defaults to the community path", &Client{BaseURL: "https://thunderstore.io"},
			"https://thunderstore.io/c/valheim/api/v1/package/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.client.listingURL(); got != tt.want {
				t.Errorf("listingURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// bareFixtureServer serves the Hexium capture at the API root only, and sends no cache
// validators at all, which is what the real host does.
func bareFixtureServer(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/hexium-v1-package-capture.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != bareListingPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestBareHostRejectsTheCommunityPath is the incompatibility itself: pointing the community
// client at a bare host reaches a 404, not a listing.
func TestBareHostRejectsTheCommunityPath(t *testing.T) {
	url := bareFixtureServer(t)

	if _, err := New(url).Sync(t.Context(), "", func(Package) error { return nil }); err == nil {
		t.Fatal("the community path resolved on a host that serves the listing at the root")
	}

	var count int
	result, err := NewBare(url).Sync(t.Context(), "", func(Package) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("bare sync: %v", err)
	}
	if count != result.Count || count == 0 {
		t.Fatalf("decoded %d packages, Count = %d", count, result.Count)
	}
}

// TestSyncWithoutValidatorsSucceedsAndRewrites is 03 §6.1: a registry that sends no ETag or
// Last-Modified re-downloads its whole listing every interval by design. Two consecutive
// syncs must both succeed and both deliver every package — an empty ETag is that host's
// normal, not a fault to report.
func TestSyncWithoutValidatorsSucceedsAndRewrites(t *testing.T) {
	url := bareFixtureServer(t)
	c := NewBare(url)

	var first, second Result
	var err error
	for i, into := range []*Result{&first, &second} {
		var count int
		// The second pass offers back whatever the first learned, which is nothing.
		*into, err = c.Sync(t.Context(), first.ETag, func(Package) error {
			count++
			return nil
		})
		if err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
		if count == 0 {
			t.Fatalf("sync %d delivered no packages", i+1)
		}
	}

	if first.ETag != "" || second.ETag != "" {
		t.Errorf("ETags = %q, %q, want empty from a host that sends none", first.ETag, second.ETag)
	}
	if first.NotModified || second.NotModified {
		t.Error("a host that sends no validators reported NotModified")
	}
	if first.Count != second.Count {
		t.Errorf("counts differ between identical syncs: %d then %d", first.Count, second.Count)
	}
}

// TestHexiumCaptureCarriesPreReleaseVersions guards the fixture itself: the corpus is only
// useful to the resolver's version handling while it still contains the shapes it was
// captured for.
func TestHexiumCaptureCarriesPreReleaseVersions(t *testing.T) {
	url := bareFixtureServer(t)

	var versions, pins int
	if _, err := NewBare(url).Sync(t.Context(), "", func(p Package) error {
		for _, v := range p.Versions {
			if strings.Contains(v.VersionNumber, "-") {
				versions++
			}
			for _, dep := range v.Dependencies {
				if strings.Contains(dep, "-beta.") {
					pins++
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if versions == 0 || pins == 0 {
		t.Fatalf("capture has %d pre-release versions and %d pre-release pins, want some of each",
			versions, pins)
	}
}
