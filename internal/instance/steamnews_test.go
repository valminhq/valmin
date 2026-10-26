package instance

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newsFixture is a trimmed GetNewsForApp answer for the game, recorded from Steam.
func newsFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/steam-news.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPatchNotesKeepOnlyTaggedPosts(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("appid"); got != "892970" {
			t.Errorf("appid = %q, want the game's client app", got)
		}
		_, _ = w.Write(newsFixture(t))
	}))
	defer server.Close()
	feed := NewPatchNotesFeed(server.Client())
	feed.baseURL = server.URL

	notes, err := feed.Notes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 {
		t.Fatal("no patch notes found in the recorded news")
	}
	for _, n := range notes {
		if strings.Contains(n.Title, "Word from the Devs") {
			t.Errorf("note %q is not a patch post", n.Title)
		}
		if !strings.HasPrefix(n.URL, "https://store.steampowered.com/news/app/892970/view/") {
			t.Errorf("note %q links to %q", n.Title, n.URL)
		}
		if strings.Contains(n.Text, "[") && strings.Contains(n.Text, "[/") {
			t.Errorf("note %q still carries markup: %q", n.Title, n.Text)
		}
	}
	if notes[0].Title != "Patch 1.0.20" {
		t.Errorf("first note = %q, want the newest patch, Patch 1.0.20", notes[0].Title)
	}
}

func TestPlainNewsText(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"heading and bold", "[h3]Patch Notes:[/h3]\n[b]Fixes:[/b]", "Patch Notes:\n\nFixes:"},
		{"list", "[list]\n[*] one\n[*] two\n[/list]", "* one\n* two"},
		{"image dropped", "before [img]{STEAM_CLAN_IMAGE}/a.png[/img] after", "before  after"},
		{"link keeps text", "see [url=https://example.com]the page[/url]", "see the page"},
		{"video dropped", "[previewyoutube=abc;full][/previewyoutube]text", "text"},
		{"blank runs", "a\n\n\n\nb", "a\n\nb"},
		{"ordered list", "[olist]\n[*] one\n[*] two\n[/olist]", "* one\n* two"},
		{"rule and strike", "[strike]old[/strike][hr][/hr] new", "old new"},
		{
			"table", "[table][tr][th]Name[/th][th]Hits[/th][/tr][tr][td]a[/td][td]1[/td][/tr][/table]",
			"Name\nHits\n\na\n1",
		},
		{"embedded link dropped", `see [dynamiclink href="x" type="clan"][/dynamiclink]text`, "see text"},
		{"literal brackets kept", "[Fixed] a crash [i think]", "[Fixed] a crash [i think]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := plainNewsText(tt.in); got != tt.want {
				t.Errorf("plainNewsText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPatchNotesAreCachedAndSurviveAFailedRefresh asserts one fetch serves the first hour and an
// outage after that returns the earlier notes rather than an error.
func TestPatchNotesAreCachedAndSurviveAFailedRefresh(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(newsFixture(t))
	}))
	defer server.Close()
	now := time.Now()
	feed := NewPatchNotesFeed(server.Client())
	feed.baseURL = server.URL
	feed.now = func() time.Time { return now }

	first, err := feed.Notes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := feed.Notes(t.Context()); err != nil || calls.Load() != 1 {
		t.Fatalf("second read: err = %v, fetches = %d, want a cached answer after 1 fetch", err, calls.Load())
	}

	now = now.Add(2 * time.Hour)
	failing.Store(true)
	stale, err := feed.Notes(t.Context())
	if err != nil {
		t.Fatalf("refresh outage = %v, want the earlier notes", err)
	}
	if len(stale) != len(first) {
		t.Errorf("stale notes = %d, want %d", len(stale), len(first))
	}

	// Steam is left alone for a few minutes after a failed refresh.
	before := calls.Load()
	if _, err := feed.Notes(t.Context()); err != nil || calls.Load() != before {
		t.Errorf("read after a failed refresh: err = %v, fetches = %d, want %d", err, calls.Load(), before)
	}
}

// TestPatchNotesLeaveSteamAloneAfterAFailureWithNothingCached asserts a feed that has never had
// notes does not ask Steam again on every request while it is down, but does after the retry delay.
func TestPatchNotesLeaveSteamAloneAfterAFailureWithNothingCached(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()
	now := time.Now()
	feed := NewPatchNotesFeed(server.Client())
	feed.baseURL = server.URL
	feed.now = func() time.Time { return now }

	for range 3 {
		if _, err := feed.Notes(t.Context()); err == nil {
			t.Fatal("Notes with Steam down and nothing cached = nil error, want one")
		}
	}
	if calls.Load() != 1 {
		t.Errorf("fetches = %d, want 1 within the retry delay", calls.Load())
	}
	now = now.Add(patchNotesRetry + time.Second)
	if _, err := feed.Notes(t.Context()); err == nil || calls.Load() != 2 {
		t.Errorf("after the retry delay: err = %v, fetches = %d, want an error and 2 fetches", err, calls.Load())
	}
}

func TestPatchNotesWithNothingCachedReportTheFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()
	feed := NewPatchNotesFeed(server.Client())
	feed.baseURL = server.URL
	if _, err := feed.Notes(t.Context()); err == nil {
		t.Fatal("Notes with Steam down and nothing cached = nil error, want one")
	}
}
