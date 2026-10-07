package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSettlePendingRestoresThePanelsValues asserts a pending copy puts its values back over
// a config the plugin rewrote, keeps what the plugin added, recreates a deleted config, and is
// removed once settled.
func TestSettlePendingRestoresThePanelsValues(t *testing.T) {
	fixture := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("testdata", "pending", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name string
		have []byte // the config after shutdown; nil when the plugin removed it
		want []byte
	}{
		{"plugin wrote its loaded values back", fixture("shutdown.cfg"), fixture("settled.cfg")},
		{"plugin left the panel's file alone", fixture("panel.cfg"), fixture("panel.cfg")},
		{"plugin removed the file", nil, fixture("panel.cfg")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := t.TempDir()
			dir := filepath.Join(server, "BepInEx", "config")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "example.cfg")
			if tt.have != nil {
				if err := os.WriteFile(path, tt.have, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path+PendingSuffix, fixture("panel.cfg"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := SettlePending(server, filepath.Join("BepInEx", "config")); err != nil {
				t.Fatalf("SettlePending: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("settled config =\n%s\nwant\n%s", got, tt.want)
			}
			if _, err := os.Stat(path + PendingSuffix); !os.IsNotExist(err) {
				t.Errorf("pending copy still present: %v", err)
			}
		})
	}
}

// TestSettlePendingWithoutAConfigDirectory asserts a vanilla server, which has no
// BepInEx/config, has nothing to settle.
func TestSettlePendingWithoutAConfigDirectory(t *testing.T) {
	if err := SettlePending(t.TempDir(), filepath.Join("BepInEx", "config")); err != nil {
		t.Fatalf("SettlePending: %v", err)
	}
}

// TestSettlePendingStaysInsideTheServer asserts a config directory the game server replaced
// with a symlink out of its tree is refused, and nothing outside is read or written.
func TestSettlePendingStaysInsideTheServer(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.cfg")
	if err := os.WriteFile(victim, []byte("[A]\nKey = original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim+PendingSuffix, []byte("[A]\nKey = planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := t.TempDir()
	if err := os.MkdirAll(filepath.Join(server, "BepInEx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(server, "BepInEx", "config")); err != nil {
		t.Fatal(err)
	}

	if err := SettlePending(server, filepath.Join("BepInEx", "config")); err == nil {
		t.Error("SettlePending followed a config directory out of the server")
	}
	if got, _ := os.ReadFile(victim); string(got) != "[A]\nKey = original\n" {
		t.Errorf("a file outside the server was rewritten: %q", got)
	}
	if _, err := os.Stat(victim + PendingSuffix); err != nil {
		t.Errorf("a pending copy outside the server was touched: %v", err)
	}
}
