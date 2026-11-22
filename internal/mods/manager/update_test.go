package manager

import (
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// TestCheckpointName asserts the setup saved before a change names the packages moving to
// another version, at most two of them, and falls back to a general name otherwise.
func TestCheckpointName(t *testing.T) {
	t.Parallel()
	installed := &store.InstanceMod{}
	update := func(name string) *stagedPackage { return &stagedPackage{fullName: name, prev: installed} }
	tests := []struct {
		name string
		pkgs []*stagedPackage
		want string
	}{
		{"new packages only", []*stagedPackage{{fullName: "Ns-New"}}, "Before changing mods"},
		{"removal only", []*stagedPackage{{fullName: "Ns-Old", prev: installed, remove: true}}, "Before changing mods"},
		{"one update", []*stagedPackage{update("Ns-A"), {fullName: "Ns-New"}}, "Before updating Ns-A"},
		{"two updates", []*stagedPackage{update("Ns-A"), update("Ns-B")}, "Before updating Ns-A and Ns-B"},
		{
			"four updates",
			[]*stagedPackage{update("Ns-A"), update("Ns-B"), update("Ns-C"), update("Ns-D")},
			"Before updating Ns-A, Ns-B and 2 more",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := checkpointName(tt.pkgs); got != tt.want {
				t.Errorf("checkpointName = %q, want %q", got, tt.want)
			}
		})
	}
}
