package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

func TestAcceptedPointsAtTheJob(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/instances", http.NoBody)
	stub := map[string]string{"job_id": "job-1", "kind": "provision", "status": "queued"}

	Accepted(rec, r, "job-1", stub)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/jobs/job-1" {
		t.Errorf("Location = %q, want the job, not the resource (11 §3)", got)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"provision"`) {
		t.Errorf("body = %s, want the job stub", rec.Body.String())
	}
}

func TestDecode(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{name: "well formed", contentType: "application/json", body: `{"name":"x"}`},
		{name: "charset parameter", contentType: "application/json; charset=utf-8", body: `{"name":"x"}`},
		{name: "no content type", body: `{"name":"x"}`},
		{
			name: "form encoded", contentType: "application/x-www-form-urlencoded",
			body: "name=x", want: "unsupported_media_type",
		},
		{name: "not json", contentType: "application/json", body: "{", want: "malformed_json"},
		// A mistyped field that silently does nothing is the failure shape to design against.
		{name: "unknown field", contentType: "application/json", body: `{"nam":"x"}`, want: "malformed_json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/instances", strings.NewReader(tt.body))
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}

			var v payload
			err := Decode(r, &v)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Decode accepted a body it should have refused")
			}
			rec := httptest.NewRecorder()
			apierr.Write(rec, r, err)
			if got := errCode(t, rec); got != tt.want {
				t.Errorf("code = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCursorRoundTrips holds ADR-035: the cursor is opaque, and a client that tampers with
// one gets invalid_parameter rather than a silently shifted page.
func TestCursorRoundTrips(t *testing.T) {
	want := Cursor{SortKey: "2026-08-30T09:00:00Z", ID: "01930f7c-6b2e-7c31-9f4a-2c1d0e8b5a77"}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?cursor="+want.Encode(), http.NoBody)
	got, present, err := ParseCursor(r)
	if err != nil || !present {
		t.Fatalf("ParseCursor: %v present=%v", err, present)
	}
	if got != want {
		t.Errorf("cursor = %+v, want %+v", got, want)
	}

	if _, present, err := ParseCursor(
		httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody),
	); err != nil ||
		present {
		t.Errorf("an absent cursor is the first page, got present=%v err=%v", present, err)
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?cursor=not-base64!!", http.NoBody)
	if _, _, err := ParseCursor(bad); err == nil {
		t.Error("a tampered cursor was accepted")
	}
}

// TestParseLimit is 11 §4: over the cap is clamped rather than rejected, because a client
// asking for too much wants as much as it can have.
func TestParseLimit(t *testing.T) {
	tests := []struct {
		query string
		want  int
		fails bool
	}{
		{query: "", want: DefaultLimit},
		{query: "?limit=10", want: 10},
		{query: "?limit=5000", want: MaxLimit},
		{query: "?limit=0", fails: true},
		{query: "?limit=-3", fails: true},
		{query: "?limit=lots", fails: true},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got, err := ParseLimit(httptest.NewRequest(http.MethodGet, "/api/v1/jobs"+tt.query, http.NoBody))
			if tt.fails {
				if err == nil {
					t.Fatalf("ParseLimit(%q) = %d, want an error", tt.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLimit(%q): %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseLimit(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

// TestEmptyPageIsAnArray holds 11 §1.1: a collection is always an object holding a list,
// and an empty list is [] rather than null, which a client would have to special-case.
func TestEmptyPageIsAnArray(t *testing.T) {
	raw, err := json.Marshal(NewPage[string](nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"items":[],"next_cursor":null,"total":null}`
	if string(raw) != want {
		t.Errorf("page = %s, want %s", raw, want)
	}
}
