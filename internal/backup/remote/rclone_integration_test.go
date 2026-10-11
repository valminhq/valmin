//go:build integration

package remote

import (
	"context"
	"encoding/json"
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

func TestRcloneReadsAddedRemoteBackUnchanged(t *testing.T) {
	binary, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone executable is unavailable")
	}
	b := &RcloneBackend{Binary: binary, ConfigFile: filepath.Join(t.TempDir(), "rclone", "rclone.conf")}
	password := "pass;word # not a comment"
	obscured, err := b.Obscure(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	revealed, err := exec.Command(binary, "reveal", obscured).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(revealed)); got != password {
		t.Fatalf("reveal(Obscure(%q)) = %q", password, got)
	}
	want := map[string]string{
		"type":              "s3",
		"provider":          "Other",
		"secret_access_key": "abc;def #ghi=jkl",
		"endpoint":          "https://s3.example.com:9000/path",
		"token":             `{"access_token":"a b","refresh_token":"r#1","expiry":"2026-01-01T00:00:00Z"}`,
		"pass":              obscured,
	}
	section := RcloneSection{Name: "s3-backups"}
	for _, key := range []string{"type", "provider", "secret_access_key", "endpoint", "token", "pass"} {
		section.Options = append(section.Options, RcloneOption{key, want[key]})
	}
	if err := b.AddRemote(section); err != nil {
		t.Fatal(err)
	}
	dump, err := exec.Command(binary, "--config", b.ConfigFile, "config", "dump").Output()
	if err != nil {
		t.Fatal(err)
	}
	var remotes map[string]map[string]string
	if err := json.Unmarshal(dump, &remotes); err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		if got := remotes["s3-backups"][key]; got != value {
			t.Errorf("rclone read %s = %q, want %q", key, got, value)
		}
	}
}

func rcloneObscure(binary, value string) (string, error) {
	out, err := exec.Command(binary, "obscure", value).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
