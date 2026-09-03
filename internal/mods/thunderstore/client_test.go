package thunderstore

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"
)

// fixtureServer serves the real capture (15 corpus packages trimmed to their
// three newest versions each — see testdata/v1-package-capture.json) and honours
// If-None-Match against the ETag it hands out, so a test can exercise the 304 path against
// real bytes rather than a synthetic one.
func fixtureServer(t *testing.T) (url, etag string) {
	t.Helper()
	body, err := os.ReadFile("testdata/v1-package-capture.json")
	if err != nil {
		t.Fatal(err)
	}
	const tag = `"fixture-etag"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", tag)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, tag
}

func TestSyncDecodesTheRealCapture(t *testing.T) {
	url, etag := fixtureServer(t)
	c := New(url)

	var pkgs []Package
	result, err := c.Sync(t.Context(), "", func(p Package) error {
		pkgs = append(pkgs, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.NotModified {
		t.Fatal("first sync (no etag sent) reported NotModified")
	}
	if result.ETag != etag {
		t.Errorf("ETag = %q, want %q", result.ETag, etag)
	}
	if len(pkgs) != 15 {
		t.Fatalf("decoded %d packages, want 15 (the corpus capture)", len(pkgs))
	}

	// F6: the OpenAPI spec's v1 PackageVersion has no "dependencies" field at all. The
	// real capture must carry real ones, or this client was written against the spec
	// instead of the measurement.
	var withDeps int
	for _, p := range pkgs {
		if latest, ok := p.Latest(); ok && len(latest.Dependencies) > 0 {
			withDeps++
		}
	}
	if withDeps == 0 {
		t.Error("no decoded package carried a non-empty dependency list")
	}
}

// TestSyncMapsFieldsNotCopiesThem is F7: mod_packages has no top-level namespace or
// description in the real response — namespace comes from "owner", and this proves the
// client actually decodes what the response calls it.
func TestSyncMapsFieldsNotCopiesThem(t *testing.T) {
	url, _ := fixtureServer(t)
	c := New(url)

	var jotunn Package
	_, err := c.Sync(t.Context(), "", func(p Package) error {
		if p.FullName == "ValheimModding-Jotunn" {
			jotunn = p
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if jotunn.FullName == "" {
		t.Fatal("ValheimModding-Jotunn not found in the fixture")
	}
	if jotunn.Owner != "ValheimModding" {
		t.Errorf("Owner = %q, want %q", jotunn.Owner, "ValheimModding")
	}
	if jotunn.RatingScore <= 0 {
		t.Errorf("RatingScore = %d, want > 0", jotunn.RatingScore)
	}
}

func TestSyncSendsIfNoneMatchAndReportsNotModified(t *testing.T) {
	url, etag := fixtureServer(t)
	c := New(url)

	result, err := c.Sync(t.Context(), etag, func(Package) error {
		t.Fatal("onPackage called on a 304 response")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.NotModified {
		t.Error("Result.NotModified = false, want true")
	}
}

func TestSyncFailsLoudlyOnSchemaMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON, two elements, and not one field this client recognises — the shape
		// an upstream field rename produces.
		_, _ = w.Write([]byte(`[{"totally_renamed":"x"},{"totally_renamed":"y"}]`))
	}))
	defer srv.Close()

	_, err := New(srv.URL).Sync(t.Context(), "", func(Package) error { return nil })
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Sync = %v, want ErrSchemaMismatch", err)
	}
}

// TestSyncFailsOnPartialSchemaMismatchAndNeverWritesTheUnnamedRows is a schema drift that
// only affects some entries — a mix of well-formed and malformed packages in the same
// response, which "every package lacks full_name" does not cover. It must still fail
// loudly, and — this is the part a coarser "any named at all" check would miss — the
// malformed entries must never reach onPackage at all: full_name is mod_packages'
// primary key, so two rows that both decoded to "" would otherwise collide and silently
// clobber each other in the upsert.
func TestSyncFailsOnPartialSchemaMismatchAndNeverWritesTheUnnamedRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"full_name":"Good-One","versions":[]},
			{"totally_renamed":"x"},
			{"full_name":"Good-Two","versions":[]}
		]`))
	}))
	defer srv.Close()

	var seen []string
	_, err := New(srv.URL).Sync(t.Context(), "", func(p Package) error {
		seen = append(seen, p.FullName)
		return nil
	})
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Sync = %v, want ErrSchemaMismatch", err)
	}
	if want := []string{"Good-One", "Good-Two"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("onPackage saw %v, want %v — the malformed entry must never reach it", seen, want)
	}
}

func TestSyncUnreachableUpstreamFails(t *testing.T) {
	// Nothing listens on TCP port 1.
	_, err := New("http://127.0.0.1:1").Sync(t.Context(), "", func(Package) error { return nil })
	if err == nil {
		t.Fatal("Sync against an unreachable upstream returned no error")
	}
}

// TestDecodeStreamProcessesOneElementAtATime proves decodeStream is genuinely streaming,
// not read-then-unmarshal wearing a streaming API: the writer goroutine below holds the
// second array element back until the first has already reached onPackage. A decoder that
// needed the whole body up front would block forever and this test would time out.
func TestDecodeStreamProcessesOneElementAtATime(t *testing.T) {
	pr, pw := io.Pipe()
	firstSeen := make(chan struct{})

	go func() {
		_, _ = pw.Write([]byte(`[`))
		_, _ = pw.Write([]byte(`{"full_name":"A-One","versions":[]}`))
		<-firstSeen
		_, _ = pw.Write([]byte(`,{"full_name":"A-Two","versions":[]}]`))
		_ = pw.Close()
	}()

	var got []string
	done := make(chan error, 1)
	go func() {
		_, _, err := decodeStream(pr, func(p Package) error {
			got = append(got, p.FullName)
			if len(got) == 1 {
				close(firstSeen)
			}
			return nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decodeStream did not return — it appears to wait for the whole body before decoding any element")
	}

	if want := []string{"A-One", "A-Two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
