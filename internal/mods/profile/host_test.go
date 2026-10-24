package profile

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func TestParseKey(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{testKey, testKey, true},
		{"  " + strings.ToUpper(testKey) + "\n", testKey, true},
		{"aaaaaaaabbbbccccddddeeeeeeeeeeee", "", false},
		{"urn:uuid:" + testKey, "", false},
		{"valmin1:abc", "", false},
		{testKey + "/../x", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseKey(tt.in)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("ParseKey(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestFetch(t *testing.T) {
	archive := []byte("zip bytes")
	stored := dataPrefix + base64.StdEncoding.EncodeToString(archive)
	tests := []struct {
		name    string
		status  int
		body    string
		max     int64
		want    string
		wantErr error
	}{
		{name: "ok", status: 200, body: stored, max: 1 << 10, want: "zip bytes"},
		{name: "trailing newline", status: 200, body: stored + "\n", max: 1 << 10, want: "zip bytes"},
		{name: "not found", status: 404, body: `{"detail":"Not found."}`, max: 1 << 10, wantErr: ErrNotFound},
		{name: "no prefix", status: 200, body: "<html>", max: 1 << 10, wantErr: ErrMalformed},
		{name: "bad base64", status: 200, body: dataPrefix + "!!!", max: 1 << 10, wantErr: ErrMalformed},
		{name: "too large", status: 200, body: stored, max: 8, wantErr: ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAgent string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAgent = r.URL.Path, r.UserAgent()
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			h := &Host{BaseURL: srv.URL + "/", Client: srv.Client()}

			got, err := h.Fetch(context.Background(), testKey, tt.max)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Fetch() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Fetch() = %q, want %q", got, tt.want)
			}
			if want := "/api/experimental/legacyprofile/get/" + testKey + "/"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if gotAgent != "valmin" {
				t.Errorf("User-Agent = %q, want valmin", gotAgent)
			}
		})
	}
}

func TestFetchReportsAnUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	h := &Host{BaseURL: srv.URL, Client: srv.Client()}
	_, err := h.Fetch(context.Background(), testKey, 1<<10)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch() error = %v, want a status error", err)
	}
}

func TestUpload(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		reply   string
		want    string
		wantErr error
	}{
		{name: "ok", status: 200, reply: `{"key":"` + testKey + `"}`, want: testKey},
		{name: "created", status: 201, reply: `{"key":"` + strings.ToUpper(testKey) + `"}`, want: testKey},
		{name: "too large", status: 413, reply: `{}`, wantErr: ErrTooLarge},
		{name: "key is not a code", status: 200, reply: `{"key":"../x"}`, wantErr: ErrMalformed},
		{name: "not json", status: 200, reply: `oops`, wantErr: ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody, gotType, gotPath, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotBody, gotType, gotPath, gotMethod = string(raw), r.Header.Get("Content-Type"), r.URL.Path, r.Method
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.reply)
			}))
			defer srv.Close()
			h := &Host{BaseURL: srv.URL, Client: srv.Client()}

			got, err := h.Upload(context.Background(), []byte("zip bytes"))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Upload() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Upload() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Upload() = %q, want %q", got, tt.want)
			}
			if want := dataPrefix + base64.StdEncoding.EncodeToString([]byte("zip bytes")); gotBody != want {
				t.Errorf("body = %q, want %q", gotBody, want)
			}
			if gotType != "application/octet-stream" || gotMethod != http.MethodPost ||
				gotPath != "/api/experimental/legacyprofile/create/" {
				t.Errorf("request = %s %s %s", gotMethod, gotPath, gotType)
			}
		})
	}
}
