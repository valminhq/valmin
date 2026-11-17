package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplayPlacesAKeptConfig: a kept entry carries the shipped hash, so a replay finds its
// bytes in the archive like any written file.
func TestReplayPlacesAKeptConfig(t *testing.T) {
	extracted := server(t, map[string]string{"config/thing.cfg": "shipped", "Thing.dll": "dll"})
	manifest := []ManifestEntry{
		{Path: "BepInEx/config/thing.cfg", SHA256: hashOf("shipped"), Kept: true},
		{Path: "BepInEx/plugins/Ns-Thing/Thing.dll", SHA256: hashOf("dll")},
	}
	dest := t.TempDir()
	if err := Replay(manifest, extracted, dest); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "BepInEx", "config", "thing.cfg"))
	if err != nil || string(body) != "shipped" {
		t.Errorf("thing.cfg = %q, %v; want the shipped bytes", body, err)
	}
}
