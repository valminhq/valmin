package cache

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func countingServer(t *testing.T, body string) (url string, requests *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests
}

func TestGetDownloadsAndCachesOnDisk(t *testing.T) {
	url, _ := countingServer(t, "zip bytes")
	c := New(t.TempDir())

	path, err := c.Get(t.Context(), "Namespace-Name-1.0.0", url, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "zip bytes" {
		t.Errorf("content = %q, want %q", got, "zip bytes")
	}
	if filepath.Base(path) != "Namespace-Name-1.0.0.zip" {
		t.Errorf("path = %s, want a file named after the ident", path)
	}
}

func TestGetReusesTheCacheWithoutASecondRequest(t *testing.T) {
	url, requests := countingServer(t, "zip bytes")
	c := New(t.TempDir())

	if _, err := c.Get(t.Context(), "ident", url, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(t.Context(), "ident", url, 0); err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1 (the second Get should have reused the cache)", n)
	}
}

// TestGetDeduplicatesConcurrentCallsForTheSameIdent is the acceptance criterion literally:
// installing the same version on three instances at once causes exactly one HTTP GET. The
// server blocks the first request until every caller has actually reached Get, so this
// cannot pass by accident (a sequential fallback would still see only one request; only a
// real race proves the dedup).
func TestGetDeduplicatesConcurrentCallsForTheSameIdent(t *testing.T) {
	const callers = 5
	var requests atomic.Int32
	release := make(chan struct{})
	arrived := make(chan struct{}, callers)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
		_, _ = w.Write([]byte("zip bytes"))
	}))
	defer srv.Close()

	c := New(t.TempDir())
	var wg sync.WaitGroup
	paths := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arrived <- struct{}{}
			paths[i], errs[i] = c.Get(t.Context(), "ident", srv.URL, 0)
		}(i)
	}
	for range callers {
		<-arrived
	}
	close(release)
	wg.Wait()

	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want exactly 1", n)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Errorf("caller %d path = %s, want %s", i, paths[i], paths[0])
		}
	}
}

func TestGetRejectsAnOversizedDownload(t *testing.T) {
	orig := MaxDownloadBytes
	MaxDownloadBytes = 4
	t.Cleanup(func() { MaxDownloadBytes = orig })

	url, _ := countingServer(t, "way more than four bytes")
	root := t.TempDir()
	c := New(root)

	if _, err := c.Get(t.Context(), "ident", url, 0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Get = %v, want ErrTooLarge", err)
	}
	assertCacheDirEmpty(t, root)
}

func TestGetVerifiesAgainstDeclaredSize(t *testing.T) {
	url, _ := countingServer(t, "nine bytes") // 10 bytes
	root := t.TempDir()
	c := New(root)

	if _, err := c.Get(t.Context(), "ident", url, 999); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Get with a wrong declared size = %v, want ErrTooLarge", err)
	}
	assertCacheDirEmpty(t, root)
}

// TestGetTruncatedDownloadCleansUpAndRedownloads: a connection that ends mid-body (the
// server declares a Content-Length it does not deliver) must not publish a corrupt file
// under the cache's real name, must not leave a stray .part behind either (this package's
// own cleanup handles the in-process failure — Sweep, tested separately, is for the case
// where the whole process dies before that cleanup runs), and a subsequent call against a
// working server must succeed.
func TestGetTruncatedDownloadCleansUpAndRedownloads(t *testing.T) {
	var attempt atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempt.Add(1) == 1 {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short"))
			return
		}
		_, _ = w.Write([]byte("complete content"))
	}))
	defer srv.Close()

	root := t.TempDir()
	c := New(root)

	if _, err := c.Get(t.Context(), "ident", srv.URL, 0); err == nil {
		t.Fatal("want an error from a truncated download")
	}
	assertCacheDirEmpty(t, root)

	path, err := c.Get(t.Context(), "ident", srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "complete content" {
		t.Errorf("content = %q, want %q", got, "complete content")
	}
}

// TestGetRejectsAnUnsafeIdent is the self-review fix: ident comes straight from a
// Thunderstore API response with no format validation anywhere upstream (03 §6.2's
// Namespace-Name-Version), so a malformed or hostile one must not be joined into a
// filesystem path unchecked — the same B5 discipline extract.go applies to a zip entry's
// name, applied here to the string that becomes this package's own filename.
func TestGetRejectsAnUnsafeIdent(t *testing.T) {
	url, requests := countingServer(t, "zip bytes")
	root := t.TempDir()
	c := New(root)

	for _, ident := range []string{
		"../../etc/passwd",
		"../escape",
		"a/b",
		`a\b`,
		"",
	} {
		if _, err := c.Get(t.Context(), ident, url, 0); !errors.Is(err, ErrInvalidIdent) {
			t.Errorf("Get(%q) = %v, want ErrInvalidIdent", ident, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("requests = %d, want 0 (a rejected ident must never reach the network)", n)
	}
	assertCacheDirEmpty(t, root)
}

func TestGetUnreachableUpstreamFails(t *testing.T) {
	c := New(t.TempDir())
	if _, err := c.Get(t.Context(), "ident", "http://127.0.0.1:1", 0); err == nil {
		t.Fatal("want an error for an unreachable upstream")
	}
}

// TestSweepRemovesOrphanedPartFilesOnly is the process-killed case Get's own cleanup
// cannot reach: a .part left behind by a panel that died mid-download is removed
// unconditionally, and a legitimate cached .zip beside it is left alone.
func TestSweepRemovesOrphanedPartFilesOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, "Some-Mod-1.0.0.zip.part")
	keep := filepath.Join(root, "Other-Mod-2.0.0.zip")
	if err := os.WriteFile(orphan, []byte("half a download"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("a complete cached zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(root); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned .part survived Sweep: err=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("Sweep touched a legitimate cached file: %v", err)
	}
}

func TestSweepOfAMissingDirectoryIsANoOp(t *testing.T) {
	if err := Sweep(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}
}

func assertCacheDirEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache directory not empty after a failed download: %v", names)
	}
}
