package errors

import (
	"encoding/json"
	stderrors "errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// registryRows reads the Code literals out of errors.go rather than a second list kept
// beside them. A second list is a second place to forget a row.
func registryRows(t *testing.T) []Code {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var rows []Code
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "Code" {
			return true
		}
		if len(lit.Elts) != 3 {
			t.Fatalf("Code literal has %d fields, want name, status, message", len(lit.Elts))
		}
		rows = append(rows, Code{
			name:    unquote(t, lit.Elts[0]),
			status:  atoi(t, lit.Elts[1]),
			message: unquote(t, lit.Elts[2]),
		})
		return true
	})
	return rows
}

func unquote(t *testing.T, e ast.Expr) string {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok {
		t.Fatalf("want a literal, got %T", e)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func atoi(t *testing.T, e ast.Expr) int {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok {
		t.Fatalf("want a literal, got %T", e)
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRegistryIsWellFormed guards the closed registry of ADR-034. A duplicate name would
// put two different failures on the wire under one code, which no client could tell apart.
func TestRegistryIsWellFormed(t *testing.T) {
	rows := registryRows(t)
	if len(rows) < 30 {
		t.Fatalf("found %d codes, want the whole 11 §2.5 table", len(rows))
	}

	seen := map[string]bool{}
	for _, c := range rows {
		if seen[c.name] {
			t.Errorf("duplicate code %q", c.name)
		}
		seen[c.name] = true

		if c.name != strings.ToLower(c.name) || strings.ContainsAny(c.name, " -.") {
			t.Errorf("code %q is not lower snake_case (11 §1.1)", c.name)
		}
		if c.message == "" {
			t.Errorf("code %q has no message; every code is renderable to any caller", c.name)
		}
		if c.status != 0 && http.StatusText(c.status) == "" {
			t.Errorf("code %q has status %d, which is not an HTTP status", c.name, c.status)
		}
	}
}

// TestJobOnlyCodesHaveNoStatus holds the 11 §2.5 note that job_runs.error_code draws from
// this table, and that a job failing has no HTTP status to report.
func TestJobOnlyCodesHaveNoStatus(t *testing.T) {
	for _, c := range []Code{Interrupted, Stalled} {
		if c.Status() != 0 {
			t.Errorf("%s has status %d, want none", c, c.Status())
		}
	}
}

// wire is the envelope as a client sees it. Code deliberately has no UnmarshalJSON: a
// closed registry that could be built from an arbitrary string would not be closed, so the
// wire form is read back as a plain string here and never as a Code.
type wire struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details"`
	RequestID string         `json:"request_id"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) wire {
	t.Helper()
	raw := rec.Body.String()
	var env struct {
		Error wire `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return env.Error
}

func TestWriteRendersTheEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "01930f7c-6b2e-7c31-9f4a-2c1d0e8b5a77")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/instances/abc/mods", http.NoBody)

	Write(rec, r, New(InstanceMustBeStopped).With("state", "running"))

	if rec.Code != 409 {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	got := decodeEnvelope(t, rec)
	if got.Code != InstanceMustBeStopped.String() {
		t.Errorf("code = %s, want instance_must_be_stopped", got.Code)
	}
	if got.RequestID != "01930f7c-6b2e-7c31-9f4a-2c1d0e8b5a77" {
		t.Errorf("request_id = %q, want the one on the response header", got.RequestID)
	}
	if got.Details["state"] != "running" {
		t.Errorf("details = %v, want state running", got.Details)
	}
}

// TestWriteKeepsTheCauseOutOfTheResponse is the D10 pairing: generic message out, full
// chain into the log. A wrapped host path is exactly what the operator needs and exactly
// what a member with one grant must not receive.
func TestWriteKeepsTheCauseOutOfTheResponse(t *testing.T) {
	const secret = "open /srv/valmin/instances/abc/worlds: permission denied"

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/instances/abc", http.NoBody)
	Write(rec, r, New(NotFound).Wrap(stderrors.New(secret)))

	if strings.Contains(rec.Body.String(), "/srv/valmin") {
		t.Errorf("response leaked the cause: %s", rec.Body.String())
	}
	if got := decodeEnvelope(t, rec); got.Message != NotFound.message {
		t.Errorf("message = %q, want the code's default", got.Message)
	}
}

// TestWriteTreatsABareErrorAsInternal covers a handler that returns something other than
// an *Error. It is a bug, and 500 with the chain in the log is how it should read.
func TestWriteTreatsABareErrorAsInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whatever", http.NoBody)
	Write(rec, r, stderrors.New("something the handler forgot to classify"))

	if rec.Code != 500 {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := decodeEnvelope(t, rec); got.Code != Internal.String() {
		t.Errorf("code = %s, want internal", got.Code)
	}
}

// TestInternalCarriesNoDetails holds 11 §2.5: internal is the only 500 and never carries
// details, so a handler that attaches one cannot leak it by accident.
func TestInternalCarriesNoDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whatever", http.NoBody)
	Write(rec, r, New(Internal).With("query", "SELECT * FROM users"))

	if got := decodeEnvelope(t, rec); got.Details != nil {
		t.Errorf("details = %v, want none", got.Details)
	}
}

// TestJobOnlyCodeOverHTTPIsInternal: a job-only code has no HTTP meaning, and answering
// with a blank status would hide the bug rather than report it.
func TestJobOnlyCodeOverHTTPIsInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/abc", http.NoBody)
	Write(rec, r, New(Interrupted))

	if rec.Code != 500 {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestValidationCollectsEveryField(t *testing.T) {
	var v Validation
	if err := v.Err(); err != nil {
		t.Fatalf("empty validation returned %v, want nil", err)
	}

	v.Add("password", FieldTooShort, "Server password must be at least 5 characters.")
	v.Add("world_name", FieldSameAsServerName, "World name must differ from the server name.")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/instances", http.NoBody)
	Write(rec, r, v.Err())

	if rec.Code != 422 {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	got := decodeEnvelope(t, rec)
	if got.Code != ValidationFailed.String() {
		t.Fatalf("code = %s, want validation_failed", got.Code)
	}
	fields, ok := got.Details["fields"].([]any)
	if !ok || len(fields) != 2 {
		t.Fatalf("details.fields = %v, want both problems in one response", got.Details["fields"])
	}
}
