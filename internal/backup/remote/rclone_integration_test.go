//go:build integration

package remote

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestRcloneWebDAVIntegration(t *testing.T) {
	binary, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone executable is unavailable")
	}
	objects := map[string][]byte{}
	collections := map[string]bool{".": true}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(path.Clean(r.URL.Path), "/dav/")
		switch r.Method {
		case "MKCOL":
			collections[key] = true
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = body
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			body, file := objects[key]
			collection := collections[key]
			if !file && !collection {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusMultiStatus)
			resourceType := ""
			if collection {
				resourceType = "<d:collection/>"
			}
			_, _ = fmt.Fprintf(
				w,
				`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:resourcetype>%s</d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`,
				r.URL.Path,
				len(body),
				resourceType,
			)
		case http.MethodDelete:
			if _, ok := objects[key]; !ok {
				http.NotFound(w, r)
				return
			}
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "httptest-ca.pem")
	if err := os.WriteFile(
		caFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", caFile)
	config := filepath.Join(t.TempDir(), "rclone.conf")
	password, err := rcloneObscure(binary, "fixture-password")
	if err != nil {
		t.Fatalf("obscure fixture password: %v", err)
	}
	contents := "[backup]\ntype = webdav\nurl = " + server.URL + "/dav\nvendor = other\nuser = fixture\npass = " + password + "\n"
	if err := os.WriteFile(config, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &RcloneBackend{Binary: binary, ConfigFile: config, Remote: "backup", Folder: "archives"}
	archive := writeArchive(t, "rclone integration archive")
	ctx := context.Background()
	put, err := backend.Put(ctx, "daily/archive.tar", archive)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := backend.Stat(ctx, put.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if stat.SizeBytes != put.SizeBytes {
		t.Fatalf("Stat size = %d; want %d", stat.SizeBytes, put.SizeBytes)
	}
	if err := backend.Delete(ctx, put.Ref); err != nil {
		t.Fatal(err)
	}
}

func rcloneObscure(binary, value string) (string, error) {
	out, err := exec.Command(binary, "obscure", value).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
