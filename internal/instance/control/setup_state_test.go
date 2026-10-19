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
