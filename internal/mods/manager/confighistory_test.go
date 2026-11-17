package manager

import "testing"

// TestHeldEdits covers when an overwritten config file counts as holding edits.
func TestHeldEdits(t *testing.T) {
	tests := []struct {
		name                         string
		dest, had, shipped, modWrote string
		want                         bool
	}{
		{"not recorded", "BepInEx/config/a.yaml", "", "s", "", false},
		{"as the old version shipped it", "BepInEx/config/a.yaml", "s", "s", "", false},
		{"as a mod last wrote it", "BepInEx/config/a.yaml", "m", "s", "m", false},
		{"edited", "BepInEx/config/a.yaml", "e", "s", "m", true},
		{"edited, no mod version recorded", "BepInEx/config/a.yaml", "e", "s", "", true},
		{"BepInEx.cfg with no mod version recorded", bepinexConfig, "e", "s", "", false},
		{"BepInEx.cfg edited after a recorded mod version", bepinexConfig, "e", "s", "m", true},
	}
	for _, tt := range tests {
		if got := heldEdits(tt.dest, tt.had, tt.shipped, tt.modWrote); got != tt.want {
			t.Errorf("%s: heldEdits = %v, want %v", tt.name, got, tt.want)
		}
	}
}
