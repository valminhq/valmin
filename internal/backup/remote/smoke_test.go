//go:build integration

package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfiguredRemoteSmoke(t *testing.T) {
	name, config := os.Getenv("REMOTE_SMOKE_REMOTE"), os.Getenv("REMOTE_SMOKE_CONFIG")
	if name == "" || config == "" {
		t.Skip("cloud smoke test requires REMOTE_SMOKE_REMOTE and REMOTE_SMOKE_CONFIG")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	b := &RcloneBackend{Binary: "rclone", ConfigFile: config, Remote: name, Folder: os.Getenv("REMOTE_SMOKE_FOLDER")}
	if err := b.CheckConfig(ctx); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "probe")
	payload := []byte("valmin remote smoke test")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("valmin-smoke/%d/probe", time.Now().UnixNano())
	obj, err := b.Put(ctx, key, file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := b.Delete(cleanupCtx, obj.Ref); err != nil {
			t.Errorf("cleanup probe: %v", err)
		}
	})
	got, err := b.Stat(ctx, obj.Ref)
	if err != nil || got.SizeBytes != int64(len(payload)) {
		t.Fatalf("remote size = %d, error = %v", got.SizeBytes, err)
	}
	if err := b.Delete(ctx, obj.Ref); err != nil {
		t.Fatal(err)
	}
}
