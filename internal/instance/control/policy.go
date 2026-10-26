package control

// ProvisionCancelPolicy stops cancellation once container creation can begin.
func ProvisionCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case "", "dirs_created", "build_cached", "cloned":
		return true, ""
	default:
		return false, "container_created"
	}
}

// CloneCancelPolicy stops cancellation after the destination container is created.
func CloneCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case "", "dirs_created", "server_cloned", "world_archived", "world_restored":
		return true, ""
	default:
		return false, "container_created"
	}
}

// GameUpdateCancelPolicy stops cancellation when the server swap starts.
func GameUpdateCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	if checkpoint == updateSwapStarted {
		return false, "the swap"
	}
	return true, ""
}

// UpdateApplyCancelPolicy stops cancellation once an apply starts changing the server: a cancel
// then would leave it stopped, or on a new game build with its old mods.
func UpdateApplyCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	if checkpoint == applyStarted {
		return false, "the update"
	}
	return true, ""
}

// UpdateCheckCancelPolicy permits cancellation throughout a read-only check.
func UpdateCheckCancelPolicy(string) (cancellable bool, phase string) { return true, "" }
