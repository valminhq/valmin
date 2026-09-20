package api

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/store"
)

// diagnosticsRouter returns a bootstrapped router, the admin's session, and a member's.
func diagnosticsRouter(t *testing.T) (rt *Router, admin, member *httptest.ResponseRecorder) {
	t.Helper()

	rt, _, admin = bootstrappedRouter(t)
	create := send(rt, authenticated(httptest.NewRequest(
		http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin))
	if create.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", create.Code, create.Body)
	}
	return rt, admin, loginAs(t, rt, "bea", "another-password")
}

// readDiagnostics fetches the report as the given session.
func readDiagnostics(t *testing.T, rt *Router, session *httptest.ResponseRecorder) *diag.Report {
	t.Helper()

	rec := send(rt, authenticated(httptest.NewRequest(
		http.MethodGet, "/api/v1/admin/diagnostics", http.NoBody), session))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET diagnostics: %d %s", rec.Code, rec.Body)
	}
	var report diag.Report
	decodeInto(t, rec, &report)
	return &report
}

// TestDiagnosticsAreAdminOnly asserts every route refuses a member. 403 rather than 404:
// panel.settings is on 09 §3.3's never-grantable list, which is the case 11 §2.3 reserves
// 403 for, and no instance's existence is disclosed by the answer.
func TestDiagnosticsAreAdminOnly(t *testing.T) {
	t.Parallel()

	rt, _, member := diagnosticsRouter(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/diagnostics"},
		{http.MethodGet, "/api/v1/admin/diagnostics/bundle"},
		{http.MethodPost, "/api/v1/admin/diagnostics/run"},
	} {
		rec := send(rt, authenticated(httptest.NewRequest(route.method, route.path, http.NoBody), member))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a member: %d, want 403", route.method, route.path, rec.Code)
		}
		if got := errCode(t, rec); got != "forbidden" {
			t.Errorf("%s %s: code %q, want forbidden", route.method, route.path, got)
		}
	}
}

// TestTheReportAnswersEveryCheckWithAProvenance asserts the page is never shown a check
// whose answer has no source, which is how a stale stamp gets read as a live pass.
func TestTheReportAnswersEveryCheckWithAProvenance(t *testing.T) {
	t.Parallel()

	rt, admin, _ := diagnosticsRouter(t)
	report := readDiagnostics(t, rt, admin)

	if len(report.Checks) == 0 {
		t.Fatal("the report has no checks")
	}
	for _, c := range report.Checks {
		if c.ID == "" || c.Group == "" || c.Title == "" {
			t.Errorf("check %+v is not identified", c)
		}
		if c.Status == "" || c.Source == "" {
			t.Errorf("%s has status %q and source %q", c.ID, c.Status, c.Source)
		}
	}
	if len(report.Migrations) == 0 {
		t.Error("the report names no applied migrations")
	}
	if report.Build.Go == "" {
		t.Error("the report carries no build identity")
	}
}

// TestTheGateChecksAreReportedFromTheirRecordedOutcome asserts a stamp written at startup
// is what the page shows, rather than the report spawning containers of its own.
func TestTheGateChecksAreReportedFromTheirRecordedOutcome(t *testing.T) {
	t.Parallel()

	rt, db, admin := bootstrappedRouter(t)
	before := readDiagnostics(t, rt, admin)
	for _, id := range []string{diag.CheckHostRoot, diag.CheckGameNetwork, diag.CheckDataRootWrite} {
		if got := checkByID(t, before, id); got.Status != diag.StatusUnknown {
			t.Errorf("%s = %s before the gate recorded anything, want unknown", id, got.Status)
		}
	}

	if err := RecordGateChecks(t.Context(), db, before.GeneratedAt); err != nil {
		t.Fatalf("record gate checks: %v", err)
	}

	after := readDiagnostics(t, rt, admin)
	for _, id := range []string{diag.CheckHostRoot, diag.CheckGameNetwork, diag.CheckDataRootWrite} {
		got := checkByID(t, after, id)
		if got.Status != diag.StatusOK {
			t.Errorf("%s = %s after the gate recorded a pass, want ok", id, got.Status)
		}
		if got.Source != diag.SourceStartup {
			t.Errorf("%s source = %s, want startup", id, got.Source)
		}
	}
}

// TestTheBundleDownloadsAsAZipAndIsAudited asserts the operator gets a file and the panel
// records who took a copy of its state.
func TestTheBundleDownloadsAsAZipAndIsAudited(t *testing.T) {
	t.Parallel()

	rt, db, admin := bootstrappedRouter(t)
	rec := send(rt, authenticated(httptest.NewRequest(
		http.MethodGet, "/api/v1/admin/diagnostics/bundle", http.NoBody), admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bundle: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "valmin-support-") {
		t.Errorf("Content-Disposition = %q", got)
	}

	body := rec.Body.Bytes()
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the response is not a zip: %v", err)
	}
	names := make([]string, 0, len(z.File))
	for _, f := range z.File {
		names = append(names, f.Name)
		r, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		if _, err := io.ReadAll(r); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		_ = r.Close()
	}
	if len(names) != 3 {
		t.Errorf("bundle entries = %v, want README.txt, report.json and config.json", names)
	}

	entries, err := db.ListAuditLog(t.Context(), store.AuditFilter{}, "", "", 10)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Detail != nil && strings.Contains(*e.Detail, "support_bundle") {
			found = true
		}
	}
	if !found {
		t.Error("taking a support bundle was not audited")
	}
}

// TestTheBundleNeverCarriesTheDataRoot asserts the handler's own wiring keeps the promise
// the bundle's README makes, against the config this panel actually runs with.
func TestTheBundleNeverCarriesTheDataRoot(t *testing.T) {
	t.Parallel()

	rt, _, admin := bootstrappedRouter(t)
	rec := send(rt, authenticated(httptest.NewRequest(
		http.MethodGet, "/api/v1/admin/diagnostics/bundle", http.NoBody), admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bundle: %d %s", rec.Code, rec.Body)
	}

	root := rt.diagnostics.Instances.Cfg.Data.Root
	body := rec.Body.Bytes()
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the response is not a zip: %v", err)
	}
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		content, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		if strings.Contains(string(content), root) {
			t.Errorf("%s carries data.root (%s)", f.Name, root)
		}
	}
}

// TestRunningTheDeepChecksReturnsAJob asserts the container-based probes are a job rather
// than work done inside a read handler.
func TestRunningTheDeepChecksReturnsAJob(t *testing.T) {
	t.Parallel()

	rt, _, admin := bootstrappedRouter(t)
	rec := send(rt, authenticated(httptest.NewRequest(
		http.MethodPost, "/api/v1/admin/diagnostics/run", http.NoBody), admin))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST run: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/api/v1/jobs/") {
		t.Errorf("Location = %q, want a job URL", got)
	}
	var job jobView
	decodeInto(t, rec, &job)
	if job.Kind != "diagnose" {
		t.Errorf("job kind = %q, want diagnose", job.Kind)
	}
}

func checkByID(t *testing.T, r *diag.Report, id string) diag.Check {
	t.Helper()
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return r.Checks[i]
		}
	}
	t.Fatalf("the report has no check %q", id)
	return diag.Check{}
}
