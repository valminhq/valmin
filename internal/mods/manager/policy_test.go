package manager

import "testing"

// TestModInstallCancelPolicy asserts that an install can be cancelled until its manifest is
// recorded, after which files are moving and the rollback path owns the outcome.
func TestModInstallCancelPolicy(t *testing.T) {
	for _, tt := range []struct {
		checkpoint string
		want       bool
	}{
		{"", true},
		{CheckpointResolved, true},
		{CheckpointDownloaded, true},
		{CheckpointStaged, true},
		{CheckpointManifestWritten, false},
		{CheckpointApplied, false},
	} {
		t.Run(tt.checkpoint, func(t *testing.T) {
			got, phase := InstallCancelPolicy(tt.checkpoint)
			if got != tt.want {
				t.Errorf("cancellable at %q = %v, want %v", tt.checkpoint, got, tt.want)
			}
			if !got && phase == "" {
				t.Error("a refusal must name the phase")
			}
		})
	}
}

// TestModUninstallCancelPolicy asserts that an uninstall cannot be cancelled at any checkpoint.
func TestModUninstallCancelPolicy(t *testing.T) {
	for _, checkpoint := range []string{"", CheckpointSaved, CheckpointRemoved} {
		if ok, phase := UninstallCancelPolicy(checkpoint); ok || phase == "" {
			t.Errorf("UninstallCancelPolicy(%q) = %v, %q; want false and a named phase",
				checkpoint, ok, phase)
		}
	}
}
