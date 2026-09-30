package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRcloneBackendContract(t *testing.T) {
	binary := fakeRclone(t)
	backend := &RcloneBackend{
		Binary:     binary,
		ConfigFile: filepath.Join(t.TempDir(), "rclone.conf"),
		Remote:     "backup",
		Folder:     "archives",
	}
	if err := os.WriteFile(backend.ConfigFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := writeArchive(t, "archive contents")
	ctx := context.Background()
	put, err := backend.Put(ctx, "daily/archive.tar", local)
	if err != nil {
		t.Fatal(err)
	}
	if put.Ref.Key != "daily/archive.tar" || put.SizeBytes != int64(len("archive contents")) {
		t.Fatalf("Put() = %#v", put)
	}
	stat, err := backend.Stat(ctx, put.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if stat.SizeBytes != 17 || stat.Ref.ProviderID != "fixture-id" {
		t.Fatalf("Stat() = %#v", stat)
	}
	if err := backend.Delete(ctx, put.Ref); err != nil {
		t.Fatal(err)
	}
}

func TestRcloneRejectsTraversalAndClassifiesExitCodes(t *testing.T) {
	binary := fakeRclone(t)
	backend := &RcloneBackend{Binary: binary, ConfigFile: filepath.Join(t.TempDir(), "rclone.conf"), Remote: "backup"}
	for _, key := range []string{"", "../escape", "/absolute", "x:y", "a\\b"} {
		if _, err := backend.Stat(context.Background(), ObjectRef{Key: key}); !errors.Is(err, ErrConfiguration) {
			t.Errorf("Stat(%q) error = %v; want ErrConfiguration", key, err)
		}
		if err := backend.Delete(context.Background(), ObjectRef{Key: key}); !errors.Is(err, ErrConfiguration) {
			t.Errorf("Delete(%q) error = %v; want ErrConfiguration", key, err)
		}
	}
	for _, tt := range []struct {
		code                int
		notFound, retryable bool
	}{{3, true, false}, {4, true, false}, {5, false, true}, {7, false, false}, {10, false, true}, {9, false, false}} {
		t.Run(strconv.Itoa(tt.code), func(t *testing.T) {
			t.Setenv("VALMIN_RCLONE_TEST_MODE", "exit:"+strconv.Itoa(tt.code))
			_, err := backend.Stat(context.Background(), ObjectRef{Key: "archive"})
			if errors.Is(err, ErrNotFound) != tt.notFound || Retryable(err) != tt.retryable {
				t.Fatalf("Stat error = %v; notFound=%v retryable=%v", err, errors.Is(err, ErrNotFound), Retryable(err))
			}
			if err != nil && strings.Contains(err.Error(), "credentials-are-secret") {
				t.Fatalf("error exposed process output: %v", err)
			}
		})
	}
}

func TestRcloneContextCancellation(t *testing.T) {
	t.Setenv("VALMIN_RCLONE_TEST_MODE", "wait")
	backend := &RcloneBackend{
		Binary:     fakeRclone(t),
		ConfigFile: filepath.Join(t.TempDir(), "rclone.conf"),
		Remote:     "backup",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := backend.Stat(ctx, ObjectRef{Key: "archive"})
	if err == nil || !Retryable(err) {
		t.Fatalf("Stat cancellation error = %v; want retryable failure", err)
	}
}

func TestRcloneTargetSeparatesArguments(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("VALMIN_RCLONE_ARGS", argsFile)
	backend := &RcloneBackend{
		Binary:     fakeRclone(t),
		ConfigFile: filepath.Join(t.TempDir(), "config with spaces"),
		Remote:     "backup",
		Folder:     "archives",
	}
	if err := os.WriteFile(backend.ConfigFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Stat(context.Background(), ObjectRef{Key: "a.tar"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "backup:archives/a.tar") || !strings.Contains(string(args), "--") {
		t.Fatalf("arguments = %q", args)
	}
}

func TestRcloneCheckConfigStoragePermissionsAndProbe(t *testing.T) {
	for _, tt := range []struct {
		name              string
		fileMode, dirMode os.FileMode
		wantErr           bool
	}{{"private", 0o600, 0o700, false}, {"group-readable-file", 0o640, 0o700, true}, {"group-accessible-directory", 0o600, 0o750, true}} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, tt.dirMode); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "rclone.conf")
			if err := os.WriteFile(config, []byte("fixture"), tt.fileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(config, tt.fileMode); err != nil {
				t.Fatal(err)
			}
			backend := &RcloneBackend{ConfigFile: config}
			err := backend.checkConfigStorage()
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkConfigStorage() error = %v; wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr {
				matches, err := filepath.Glob(filepath.Join(dir, ".valmin-write-check-*"))
				if err != nil {
					t.Fatal(err)
				}
				if len(matches) != 0 {
					t.Fatalf("writability probe left files behind: %v", matches)
				}
			}
		})
	}
}

func TestRcloneMissingBinaryErrorIsSanitized(t *testing.T) {
	backend := &RcloneBackend{
		Binary:     filepath.Join(t.TempDir(), "missing-rclone"),
		ConfigFile: "secret-config-path",
		Remote:     "backup",
	}
	_, err := backend.Stat(context.Background(), ObjectRef{Key: "archive"})
	if err == nil {
		t.Fatal("Stat succeeded with a missing executable")
	}
	if strings.Contains(err.Error(), backend.Binary) || strings.Contains(err.Error(), backend.ConfigFile) {
		t.Fatalf("error exposed a local path: %v", err)
	}
}

func TestRcloneCheckConfigPersistsRefresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(config, []byte("[backup]\ntype = webdav\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VALMIN_RCLONE_TEST_MODE", "refresh")
	backend := &RcloneBackend{Binary: fakeRclone(t), ConfigFile: config, Remote: "backup"}
	if err := backend.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "refreshed-token") {
		t.Fatalf("config after fake refresh = %q; expected persisted token", contents)
	}
}

func fakeRclone(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "rclone-fixture")
	script := `#!/bin/sh
config_file=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "--config" ]; then config_file=$arg; fi
  previous=$arg
done
if [ -n "$VALMIN_RCLONE_ARGS" ]; then printf '%s\n' "$@" > "$VALMIN_RCLONE_ARGS"; fi
case "$VALMIN_RCLONE_TEST_MODE" in
  exit:*) printf '%s\n' 'credentials-are-secret' >&2; exit "${VALMIN_RCLONE_TEST_MODE#exit:}" ;;
  wait) sleep 10; exit 0 ;;
  refresh) printf '%s\n' 'refreshed-token' >> "$config_file" ;;
esac
case "$*" in
  *lsjson*) printf '%s' '{"Size":17,"ID":"fixture-id","IsDir":false}' ;;
  *listremotes*) printf '%s\n' 'backup:' ;;
esac
exit 0
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}
