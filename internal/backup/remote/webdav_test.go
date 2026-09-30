package remote

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWebDAVBackendContract(t *testing.T) {
	objects := map[string][]byte{}
	var methods []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if got := r.Header.Get(
			"Authorization",
		); got != "Basic "+base64.StdEncoding.EncodeToString(
			[]byte("alice:sensitive-password"),
		) {
			t.Errorf("Authorization = %q; want configured credentials", got)
		}
		key := strings.TrimPrefix(r.URL.Path, "/dav/")
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodHead:
			body, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if _, ok := objects[key]; !ok {
				http.NotFound(w, r)
				return
			}
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	backend := &WebDAVBackend{
		URL:       server.URL + "/dav",
		Username:  "alice",
		Password:  "sensitive-password",
		Transport: server.Client().Transport,
	}
	local := writeArchive(t, "archive contents")
	ctx := context.Background()

	got, err := backend.Put(ctx, "daily/archive.tar", local)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref.Key != "daily/archive.tar" || got.SizeBytes != int64(len("archive contents")) {
		t.Fatalf("Put() = %#v", got)
	}
	stat, err := backend.Stat(ctx, got.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if stat.SizeBytes != got.SizeBytes {
		t.Fatalf("Stat size = %d; want %d", stat.SizeBytes, got.SizeBytes)
	}
	if err := backend.Delete(ctx, got.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Stat(ctx, got.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete error = %v; want ErrNotFound", err)
	}
	if !strings.Contains(strings.Join(methods, ","), "PUT") || !strings.Contains(strings.Join(methods, ","), "HEAD") ||
		!strings.Contains(strings.Join(methods, ","), "DELETE") {
		t.Fatalf("methods = %v", methods)
	}
}

func TestWebDAVRejectsTraversalAndRedactsFailures(t *testing.T) {
	const secret = "body-secret-password"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/dav/leak" {
			http.Redirect(w, r, "https://elsewhere.invalid/"+secret, http.StatusFound)
			return
		}
		w.Header().Set("Location", "https://elsewhere.invalid/"+secret)
		http.Error(w, secret, http.StatusInternalServerError)
	}))
	defer server.Close()
	backend := &WebDAVBackend{
		URL:       server.URL + "/dav",
		Username:  "user",
		Password:  secret,
		Transport: server.Client().Transport,
	}
	for _, key := range []string{"", "../escape", "/absolute", "x:y", "a\\b"} {
		if _, err := backend.Stat(context.Background(), ObjectRef{Key: key}); !errors.Is(err, ErrConfiguration) {
			t.Errorf("Stat(%q) error = %v; want ErrConfiguration", key, err)
		}
	}
	if err := backend.Delete(context.Background(), ObjectRef{}); !errors.Is(err, ErrConfiguration) {
		t.Errorf("Delete(empty key) error = %v; want ErrConfiguration", err)
	}
	if requests != 0 {
		t.Fatalf("invalid object references made %d requests", requests)
	}
	_, err := backend.Stat(context.Background(), ObjectRef{Key: "failure"})
	if err == nil || !Retryable(err) {
		t.Fatalf("Stat server failure = %v; want retryable failure", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked response secret: %v", err)
	}
	_, err = backend.Stat(context.Background(), ObjectRef{Key: "leak"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("redirect error = %v; want redacted error", err)
	}
}

func TestWebDAVRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }),
	)
	defer server.Close()
	backend := &WebDAVBackend{URL: server.URL, Transport: server.Client().Transport}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := backend.Stat(ctx, ObjectRef{Key: "archive"}); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach server")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !Retryable(err) {
			t.Fatalf("Stat cancellation error = %v; want retryable failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stat did not return after cancellation")
	}
}

func writeArchive(t *testing.T, contents string) string {
	t.Helper()
	file := path.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestWebDAVAddressPolicy(t *testing.T) {
	backend := &WebDAVBackend{}
	for _, addr := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "192.168.1.20", "169.254.1.2"} {
		if backend.allowed(netip.MustParseAddr(addr)) {
			t.Errorf("allowed(%s) = true; want denied", addr)
		}
	}
	if !backend.allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("allowed(public address) = false; want true")
	}
	backend.AllowedPrivate = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	if !backend.allowed(netip.MustParseAddr("192.168.1.20")) {
		t.Error("allowed(configured private address) = false; want true")
	}
	if backend.allowed(netip.MustParseAddr("192.168.2.20")) {
		t.Error("allowed(private address outside configured prefix) = true; want false")
	}
}
