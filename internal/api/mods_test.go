package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

// fixtureModsServer serves the real Thunderstore capture WP-M2-02 shares with the
// thunderstore package's own tests, so the runner is proven end to end against real
// response bytes rather than a hand-built one.
func fixtureModsServer(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../mods/thunderstore/testdata/v1-package-capture.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture-etag"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// modsFixture builds a Mods against a fresh DB and testEngine (router_test.go's shared
// engine builder). baseURL "" points the client at fixtureModsServer's real capture; any
// other value — e.g. an unreachable address — is used verbatim, for the failure tests.
func modsFixture(t *testing.T, baseURL string) (*Mods, *store.DB) {
	t.Helper()
	h, _ := health(t)
	cfg := config.Defaults()
	if baseURL == "" {
		baseURL = fixtureModsServer(t)
	}
	return &Mods{DB: h.DB, Engine: testEngine(h.DB, &cfg), Client: thunderstore.New(baseURL)}, h.DB
}

// submitSync submits thunderstore_sync exactly as enqueueSync does and waits for it to
// reach a terminal status — the one submission shape every test below needs.
func submitSync(t *testing.T, m *Mods) *store.Job {
	t.Helper()
	job, err := m.Engine.Submit(t.Context(), syncSpec(), m.syncRun)
	if err != nil {
		t.Fatal(err)
	}
	return waitForModsSyncTerminal(t, m.DB, job.ID)
}

func waitForModsSyncTerminal(t *testing.T, db *store.DB, jobID string) *store.Job {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		j, err := db.JobByID(t.Context(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if j != nil && j.Status != "running" {
			return j
		}
		select {
		case <-deadline:
			t.Fatal("thunderstore_sync never reached a terminal status")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSyncRunPopulatesTheIndex is WP-M2-02's end-to-end acceptance test: submitting
// thunderstore_sync against the real capture lands every one of the 15 corpus packages in
// mod_packages and mod_versions, with the ETag and synced_at recorded for next time.
func TestSyncRunPopulatesTheIndex(t *testing.T) {
	m, db := modsFixture(t, "")
	ctx := t.Context()

	if final := submitSync(t, m); final.Status != "succeeded" {
		t.Fatalf("job status = %q, error = %v", final.Status, final.ErrorCode)
	}

	got, err := db.ModPackageByFullName(ctx, "ValheimModding-Jotunn")
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "ValheimModding" {
		t.Errorf("Namespace = %q, want %q", got.Namespace, "ValheimModding")
	}
	if got.LatestVersion != "2.29.2" {
		t.Errorf("LatestVersion = %q, want %q", got.LatestVersion, "2.29.2")
	}
	if got.Rating <= 0 {
		t.Errorf("Rating = %d, want > 0 (F7: must come from rating_score, not be left null)", got.Rating)
	}

	versions, err := db.ModVersionsByFullName(ctx, "ValheimModding-Jotunn")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("no mod_versions rows for ValheimModding-Jotunn")
	}

	var etag string
	if ok, err := db.KVGet(ctx, kvThunderstoreETag, &etag); err != nil || !ok {
		t.Fatalf("kv etag: ok=%v err=%v", ok, err)
	}
	if etag != `"fixture-etag"` {
		t.Errorf("stored etag = %q, want %q", etag, `"fixture-etag"`)
	}
}

// TestSyncRunSecondPassIsNotModified proves the cached ETag round-trips through kv and is
// actually sent back: syncing twice against the same fixture gets a 304 the second time.
func TestSyncRunSecondPassIsNotModified(t *testing.T) {
	m, _ := modsFixture(t, "")

	if first := submitSync(t, m); first.Status != "succeeded" {
		t.Fatalf("first sync status = %q", first.Status)
	}
	if second := submitSync(t, m); second.Status != "succeeded" {
		t.Fatalf("second sync status = %q", second.Status)
	}
}

// TestEnqueueSyncSkipsAConcurrentRun is ADR-030 for the first global-scoped job kind in
// the codebase: a second submission on the same lock key must not queue or error loudly —
// it is skipped, and the running job is left to finish.
func TestEnqueueSyncSkipsAConcurrentRun(t *testing.T) {
	m, db := modsFixture(t, "")

	release := make(chan struct{})
	started := make(chan struct{})
	// A hand-rolled blocking Runner on syncSpec()'s own lock key, so the lock is
	// deliberately held open when enqueueSync tries a second submission — m.syncRun
	// itself would finish before the assertion below ever ran.
	first, err := m.Engine.Submit(t.Context(), syncSpec(), func(context.Context, *jobs.Handle) jobs.Outcome {
		close(started)
		<-release
		return jobs.Outcome{Status: "succeeded"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	m.enqueueSync(t.Context()) // must not panic, block, or submit a second job

	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = ?`, jobs.KindThunderstoreSync.String(),
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("job_runs rows for thunderstore_sync = %d, want 1 (the running one, not a second)", count)
	}

	close(release)
	waitForModsSyncTerminal(t, db, first.ID)
}

// TestSyncRunFailsLoudlyLeavesIndexUntouched: an unreachable upstream fails the job and
// changes nothing already synced — a bad sync must not blank out a good index.
func TestSyncRunFailsLoudlyLeavesIndexUntouched(t *testing.T) {
	m, db := modsFixture(t, "http://127.0.0.1:1") // nothing listens here
	ctx := t.Context()

	if err := db.UpsertModPackages(ctx,
		[]store.ModPackage{{FullName: "Preexisting-Package", Namespace: "Preexisting", Name: "Package"}}, nil,
	); err != nil {
		t.Fatal(err)
	}

	if final := submitSync(t, m); final.Status != "failed" {
		t.Fatalf("job status = %q, want failed", final.Status)
	}

	got, err := db.ModPackageByFullName(ctx, "Preexisting-Package")
	if err != nil {
		t.Fatal(err)
	}
	if got.FullName != "Preexisting-Package" {
		t.Error("a failed sync removed a package that predated it")
	}
}

// TestSyncRunIsBoundedByATimeout: syncRun's ctx is cancelled only on lease loss, and the
// lease is renewed by a goroutine that runs independently of this Runner's own progress
// (12 §5.2) — so without syncTimeout, a stalled upstream that never finishes responding
// hangs the job, and the global thunderstore_sync lock with it, indefinitely. Proven
// against a server that writes the opening "[" and then blocks for good, with syncTimeout
// shrunk so the test does not wait out the real thirty minutes.
func TestSyncRunIsBoundedByATimeout(t *testing.T) {
	orig := syncTimeout
	syncTimeout = 50 * time.Millisecond
	t.Cleanup(func() { syncTimeout = orig })

	// close(block) must run before srv.Close(), which blocks until the in-flight handler
	// below returns — t.Cleanup runs LIFO, so this is registered second to run first.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block // never responds further; the request's ctx is what ends this
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	h, _ := health(t)
	cfg := config.Defaults()
	m := &Mods{DB: h.DB, Engine: testEngine(h.DB, &cfg), Client: thunderstore.New(srv.URL)}

	final := submitSync(t, m)
	if final.Status != "failed" {
		t.Fatalf("job status = %q, want failed (a stalled upstream must not hang the job)", final.Status)
	}
}

// TestToStoreRowsMapsFieldsNotCopiesThem is F7 at the api-layer boundary: Namespace and
// Rating must come from Owner/RatingScore, and Description/LatestVersion/IconURL from the
// computed latest version — none of these exist as top-level fields in the real response.
func TestToStoreRowsMapsFieldsNotCopiesThem(t *testing.T) {
	p := thunderstore.Package{
		FullName: "A-B", Owner: "A", Name: "B", RatingScore: 42, Categories: []string{"Mods"},
		Versions: []thunderstore.Version{
			{VersionNumber: "1.0.0", Description: "old", Downloads: 1},
			{VersionNumber: "2.0.0", Description: "new", Icon: "https://x/icon.png", Downloads: 2},
		},
	}
	row, versions, err := toStoreRows(&p)
	if err != nil {
		t.Fatal(err)
	}
	if row.Namespace != "A" {
		t.Errorf("Namespace = %q, want %q", row.Namespace, "A")
	}
	if row.Rating != 42 {
		t.Errorf("Rating = %d, want 42", row.Rating)
	}
	if row.LatestVersion != "2.0.0" || row.Description != "new" || row.IconURL != "https://x/icon.png" {
		t.Errorf("latest-derived fields = %+v, want the 2.0.0 version's", row)
	}
	if row.Downloads != 3 {
		t.Errorf("Downloads = %d, want 3 (summed across versions)", row.Downloads)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
}
