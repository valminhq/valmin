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
			dir := t.TempDir()
			path := filepath.Join(dir, "example.cfg")
			if tt.have != nil {
				if err := os.WriteFile(path, tt.have, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path+PendingSuffix, fixture("panel.cfg"), 0o600); err != nil {
				t.Fatal(err)
			}

			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := SettlePending(root); err != nil {
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
