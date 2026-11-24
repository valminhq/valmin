package control

import (
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// TestSameSetup asserts setups match on build, launch, mods and config files, whatever the mod
// order, and a light setup never stands in for a full one.
func TestSameSetup(t *testing.T) {
	t.Parallel()
	row := func(build, snapshot string, configs ...store.SetupConfigRef) *store.SavedSetup {
		return &store.SavedSetup{GameBuildID: build, SnapshotJSON: snapshot, Configs: configs}
	}
	const (
		modsAB  = `{"config_tree":true,"mods":[{"full_name":"A","version":"1"},{"full_name":"B","version":"1"}]}`
		modsBA  = `{"config_tree":true,"mods":[{"full_name":"B","version":"1"},{"full_name":"A","version":"1"}]}`
		lightAB = `{"config_tree":true,"light":true,"mods":[{"full_name":"A","version":"1"},{"full_name":"B","version":"1"}]}`
		newerA  = `{"config_tree":true,"mods":[{"full_name":"A","version":"2"},{"full_name":"B","version":"1"}]}`
		public  = `{"config_tree":true,"instance":{"public":true},"mods":[]}`
		private = `{"config_tree":true,"instance":{"public":false},"mods":[]}`
		legacy  = `{"mods":[]}`
	)
	cfg := store.SetupConfigRef{File: "a.cfg", SHA256: "1", SizeBytes: 1}
	edited := store.SetupConfigRef{File: "a.cfg", SHA256: "2", SizeBytes: 1}
	for _, tc := range []struct {
		name          string
		capture, prev *store.SavedSetup
		want          bool
	}{
		{"mods in another order", row("1", modsAB), row("1", modsBA), true},
		{"full capture, light newest", row("1", modsAB), row("1", lightAB), false},
		{"light capture, full newest", row("1", lightAB), row("1", modsAB), true},
		{"other mod version", row("1", newerA), row("1", modsAB), false},
		{"other game build", row("2", modsAB), row("1", modsAB), false},
		{"other launch settings", row("1", public), row("1", private), false},
		{"same config files", row("1", modsAB, cfg), row("1", modsAB, cfg), true},
		{"edited config file", row("1", modsAB, edited), row("1", modsAB, cfg), false},
		{"newest without the config tree", row("1", private), row("1", legacy), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sameSetup(tc.capture, tc.prev); got != tc.want {
				t.Errorf("sameSetup = %v, want %v", got, tc.want)
			}
		})
	}
}
