package control

import "testing"

// TestGameUpdateIsCancellableOnlyBeforeTheSwap asserts that a game update can be cancelled
// at every checkpoint before the swap and is refused once the swap has begun.
func TestGameUpdateIsCancellableOnlyBeforeTheSwap(t *testing.T) {
	tests := []struct {
		checkpoint string
		want       bool
	}{
		{"", true},
		{updatePreBackupTaken, true},
		{updateBuildCached, true},
		{updateCloned, true},
		{updateModsReplayed, true},
		{updateSwapStarted, false},
	}
	for _, tt := range tests {
		t.Run("at "+tt.checkpoint, func(t *testing.T) {
			got, phase := GameUpdateCancelPolicy(tt.checkpoint)
			if got != tt.want {
				t.Errorf("cancellable at %q = %v, want %v", tt.checkpoint, got, tt.want)
			}
			if !got && phase == "" {
				t.Error("a refusal must name the phase it refuses in")
			}
		})
	}
}

// TestCloneCancelPolicyClosesAtContainerCreation asserts that a clone can be cancelled until
// its destination container exists.
func TestCloneCancelPolicyClosesAtContainerCreation(t *testing.T) {
	for _, checkpoint := range []string{"", "dirs_created", "server_cloned", "world_archived", "world_restored"} {
		if ok, phase := CloneCancelPolicy(checkpoint); !ok || phase != "" {
			t.Errorf("CloneCancelPolicy(%q) = %v, %q; want cancellable", checkpoint, ok, phase)
		}
	}
	if ok, phase := CloneCancelPolicy("container_created"); ok || phase != "container_created" {
		t.Errorf("CloneCancelPolicy(container_created) = %v, %q; want false, container_created", ok, phase)
	}
}
