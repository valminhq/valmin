package control

import (
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// TestLaunchOfRefusesUndecodableModifiers asserts a row whose modifiers do not decode fails
// instead of exporting a launch without them.
func TestLaunchOfRefusesUndecodableModifiers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, modifiers string
		wantErr         bool
	}{
		{"none", "", false},
		{"valid", `{"combat":"hard"}`, false},
		{"broken", `{"combat":`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LaunchOf(&store.Instance{ID: "inst-a", Modifiers: &tc.modifiers})
			if (err != nil) != tc.wantErr {
				t.Errorf("LaunchOf(%q) error = %v, want error %v", tc.modifiers, err, tc.wantErr)
			}
		})
	}
}

// TestModsFingerprint asserts the fingerprint ignores mod order, registry, side and lock, and
// changes with a mod's version, enabled state, or a mod added.
func TestModsFingerprint(t *testing.T) {
	t.Parallel()
	a := SetupMod{FullName: "Ns-A", Source: "thunderstore", Version: "1.0.0", Enabled: true}
	b := SetupMod{FullName: "Ns-B", Source: "thunderstore", Version: "2.0.0", Enabled: true}
	base := ModsFingerprint([]SetupMod{a, b})
	moved := a
	moved.Source, moved.Side, moved.Locked = "hexium", "client_required", true
	newer, disabled := a, a
	newer.Version = "1.1.0"
	disabled.Enabled = false
	for _, tc := range []struct {
		name string
		mods []SetupMod
		same bool
	}{
		{"reordered", []SetupMod{b, a}, true},
		{"registry, side and lock", []SetupMod{moved, b}, true},
		{"version", []SetupMod{newer, b}, false},
		{"disabled", []SetupMod{disabled, b}, false},
		{"removed", []SetupMod{b}, false},
	} {
		if got := ModsFingerprint(tc.mods) == base; got != tc.same {
			t.Errorf("%s: same fingerprint = %v, want %v", tc.name, got, tc.same)
		}
	}
}
