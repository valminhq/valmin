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

// TestSettlePendingTreeReachesSubdirectories asserts the tree settle applies a pending copy in a
// subdirectory, which the flat settle leaves alone.
func TestSettlePendingTreeReachesSubdirectories(t *testing.T) {
	for _, tt := range []struct {
		name    string
		settle  func(*os.Root) error
		settled bool
	}{
		{"flat", SettlePending, false},
		{"tree", SettlePendingTree, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sub := filepath.Join(dir, "mod", "extra")
			if err := os.MkdirAll(sub, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sub, "x.cfg")
			if err := os.WriteFile(path, []byte("[A]\nk = plugin\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+PendingSuffix, []byte("[A]\nk = panel\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := tt.settle(root); err != nil {
				t.Fatalf("settle: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if settled := string(got) == "[A]\nk = panel\n"; settled != tt.settled {
				t.Errorf("config = %q, settled %v, want settled %v", got, settled, tt.settled)
			}
			if _, err := os.Stat(path + PendingSuffix); os.IsNotExist(err) == !tt.settled {
				t.Errorf("pending copy present = %v after a %s settle", err == nil, tt.name)
			}
		})
	}
}
